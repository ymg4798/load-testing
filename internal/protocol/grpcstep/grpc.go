// Package grpcstep is the "grpc" step: one unary call per step, with the
// message written as YAML/JSON. Method types come from .proto files or
// from the server's reflection service, so no code generation is needed.
//
//   - grpc:
//     name: getUser
//     target: localhost:50051
//     method: user.v1.UserService/GetUser
//     proto: protos/user.proto        # or reflect: true
//     importPaths: [protos]
//     message: { id: "{{ users.id }}" }
//     metadata: { authorization: "Bearer {{ token }}" }
//     tls: false
//     conns: 4                        # HTTP/2 connections to spread calls over
//     check: { code: OK, json: { "$.name": "kim" } }
//     extract: { name: $.name }
package grpcstep

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bufbuild/protocompile"
	"github.com/bufbuild/protocompile/linker"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"

	"github.com/ymg4798/lt/internal/protocol"
	"github.com/ymg4798/lt/internal/tmpl"
	"github.com/ymg4798/lt/internal/yamlx"
)

func init() { protocol.Register("grpc", compile) }

type checkSpec struct {
	Code         any            `yaml:"code"`
	BodyContains string         `yaml:"bodyContains"`
	JSON         map[string]any `yaml:"json"`
	MaxLatency   yamlx.Duration `yaml:"maxLatency"`
}

type spec struct {
	Name        string            `yaml:"name"`
	Target      string            `yaml:"target"`
	Method      string            `yaml:"method"`
	Proto       string            `yaml:"proto"`
	ImportPaths []string          `yaml:"importPaths"`
	Reflect     bool              `yaml:"reflect"`
	Message     any               `yaml:"message"`
	Metadata    map[string]string `yaml:"metadata"`
	TLS         bool              `yaml:"tls"`
	Conns       int               `yaml:"conns"`
	Timeout     yamlx.Duration    `yaml:"timeout"`
	Check       *checkSpec        `yaml:"check"`
	Extract     map[string]string `yaml:"extract"`
}

type step struct {
	tracing  protocol.Tracing
	name     string
	fullName string // "/pkg.Service/Method"
	in, out  protoreflect.MessageDescriptor
	message  tmpl.Value
	md       map[string]*tmpl.String
	timeout  time.Duration
	codes    map[codes.Code]bool
	check    *protocol.Check
	extract  *protocol.Extract
	pool     *connPool
}

type connPool struct {
	conns []*grpc.ClientConn
	next  atomic.Uint64
}

func (p *connPool) get() *grpc.ClientConn {
	return p.conns[p.next.Add(1)%uint64(len(p.conns))]
}

func compile(raw any, env *protocol.Env) (protocol.Step, error) {
	var sp spec
	if err := yamlx.Decode(raw, &sp); err != nil {
		return nil, fmt.Errorf("grpc: %w", err)
	}
	if sp.Target == "" || sp.Method == "" {
		return nil, fmt.Errorf("grpc: target and method are required")
	}
	svc, meth, ok := strings.Cut(strings.TrimPrefix(sp.Method, "/"), "/")
	if !ok {
		return nil, fmt.Errorf("grpc: method must look like package.Service/Method")
	}
	s := &step{name: sp.Name, fullName: "/" + svc + "/" + meth, timeout: sp.Timeout.D(), tracing: env.Options.Tracing}
	if s.name == "" {
		s.name = meth
	}
	if s.timeout == 0 {
		s.timeout = env.Options.Timeout
	}
	if sp.Conns <= 0 {
		sp.Conns = 4
	}

	poolKey := fmt.Sprintf("grpc:%s:%v:%d", sp.Target, sp.TLS, sp.Conns)
	p, err := env.Shared(poolKey, func() (any, error) {
		creds := insecure.NewCredentials()
		if sp.TLS {
			creds = credentials.NewTLS(&tls.Config{InsecureSkipVerify: env.Options.Insecure}) //nolint:gosec // opt-in
		}
		pool := &connPool{}
		for i := 0; i < sp.Conns; i++ {
			c, err := grpc.NewClient(sp.Target, grpc.WithTransportCredentials(creds))
			if err != nil {
				return nil, err
			}
			pool.conns = append(pool.conns, c)
		}
		env.OnClose(func() {
			for _, c := range pool.conns {
				c.Close()
			}
		})
		return pool, nil
	})
	if err != nil {
		return nil, fmt.Errorf("grpc %s: %w", s.name, err)
	}
	s.pool = p.(*connPool)

	var md protoreflect.MethodDescriptor
	switch {
	case sp.Proto != "":
		md, err = fromProto(env, sp.Proto, sp.ImportPaths, svc, meth)
	case sp.Reflect:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		md, err = fromReflection(ctx, s.pool.conns[0], svc, meth)
		cancel()
	default:
		err = fmt.Errorf("give proto: <file> or reflect: true")
	}
	if err != nil {
		return nil, fmt.Errorf("grpc %s: %w", s.name, err)
	}
	if md.IsStreamingClient() || md.IsStreamingServer() {
		return nil, fmt.Errorf("grpc %s: only unary methods are supported", s.name)
	}
	s.in, s.out = md.Input(), md.Output()

	if sp.Message == nil {
		sp.Message = map[string]any{}
	}
	if s.message, err = tmpl.CompileValue(sp.Message); err != nil {
		return nil, err
	}
	s.md = map[string]*tmpl.String{}
	for k, v := range sp.Metadata {
		if s.md[strings.ToLower(k)], err = tmpl.Compile(v); err != nil {
			return nil, err
		}
	}
	cs := &protocol.CheckSpec{}
	var codeSpec any = "OK"
	if sp.Check != nil {
		if sp.Check.Code != nil {
			codeSpec = sp.Check.Code
		}
		cs.BodyContains, cs.JSON, cs.MaxLatency = sp.Check.BodyContains, sp.Check.JSON, sp.Check.MaxLatency
	}
	if s.codes, err = parseCodes(codeSpec); err != nil {
		return nil, fmt.Errorf("grpc %s: %w", s.name, err)
	}
	if s.check, err = protocol.CompileCheck(cs); err != nil {
		return nil, err
	}
	if s.extract, err = protocol.CompileExtract(sp.Extract); err != nil {
		return nil, err
	}
	return s, nil
}

func parseCodes(v any) (map[codes.Code]bool, error) {
	out := map[codes.Code]bool{}
	var list []any
	if l, ok := v.([]any); ok {
		list = l
	} else {
		list = []any{v}
	}
	for _, x := range list {
		switch c := x.(type) {
		case int:
			out[codes.Code(c)] = true
		case string:
			found := false
			for i := codes.OK; i <= codes.Unauthenticated; i++ {
				if strings.EqualFold(i.String(), c) || strings.EqualFold(strings.ReplaceAll(i.String(), "_", ""), c) {
					out[i], found = true, true
				}
			}
			if !found {
				if n, err := strconv.Atoi(c); err == nil {
					out[codes.Code(n)], found = true, true
				}
			}
			if !found {
				return nil, fmt.Errorf("unknown status code %q", c)
			}
		default:
			return nil, fmt.Errorf("bad status code %v", x)
		}
	}
	return out, nil
}

func fromProto(env *protocol.Env, file string, imports []string, svc, meth string) (protoreflect.MethodDescriptor, error) {
	paths := []string{env.BaseDir}
	for _, p := range imports {
		if !filepath.IsAbs(p) {
			p = filepath.Join(env.BaseDir, p)
		}
		paths = append(paths, p)
	}
	rel := file
	if filepath.IsAbs(file) {
		paths = append(paths, filepath.Dir(file))
		rel = filepath.Base(file)
	} else {
		// Let imports inside the file resolve relative to its folder too.
		paths = append(paths, filepath.Join(env.BaseDir, filepath.Dir(file)))
	}
	key := "proto:" + file + ":" + strings.Join(paths, ",")
	v, err := env.Shared(key, func() (any, error) {
		c := protocompile.Compiler{
			Resolver: protocompile.WithStandardImports(&protocompile.SourceResolver{ImportPaths: paths}),
		}
		return c.Compile(context.Background(), rel)
	})
	if err != nil {
		return nil, err
	}
	for _, f := range v.(linker.Files) {
		if md := findMethod(f, svc, meth, map[string]bool{}); md != nil {
			return md, nil
		}
	}
	return nil, fmt.Errorf("%s/%s not found in %s", svc, meth, file)
}

// findMethod looks for a method in a file and the files it imports.
func findMethod(fd protoreflect.FileDescriptor, svc, meth string, seen map[string]bool) protoreflect.MethodDescriptor {
	if seen[fd.Path()] {
		return nil
	}
	seen[fd.Path()] = true
	services := fd.Services()
	for i := 0; i < services.Len(); i++ {
		sd := services.Get(i)
		if string(sd.FullName()) == svc {
			return sd.Methods().ByName(protoreflect.Name(meth))
		}
	}
	imports := fd.Imports()
	for i := 0; i < imports.Len(); i++ {
		if md := findMethod(imports.Get(i).FileDescriptor, svc, meth, seen); md != nil {
			return md
		}
	}
	return nil
}

func (s *step) Run(ctx context.Context, vu *protocol.VU) error {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	v, err := s.message.Render(vu)
	if err != nil {
		return protocol.Fail(s.name, "template", err.Error())
	}
	js, err := json.Marshal(v)
	if err != nil {
		return protocol.Fail(s.name, "template", err.Error())
	}
	req := dynamicpb.NewMessage(s.in)
	if err := protojson.Unmarshal(js, req); err != nil {
		return protocol.Fail(s.name, "grpc:bad_message", err.Error())
	}
	if len(s.md) > 0 {
		pairs := make([]string, 0, len(s.md)*2)
		for k, t := range s.md {
			val, err := t.RenderString(vu)
			if err != nil {
				return protocol.Fail(s.name, "template", err.Error())
			}
			pairs = append(pairs, k, val)
		}
		ctx = metadata.AppendToOutgoingContext(ctx, pairs...)
	}
	traceID := ""
	if tr := s.tracing.NewTrace(vu); tr != nil {
		traceID = tr.ID
		for _, h := range tr.Headers {
			ctx = metadata.AppendToOutgoingContext(ctx, h[0], h[1])
		}
	}
	resp := dynamicpb.NewMessage(s.out)
	start := time.Now()
	err = s.pool.get().Invoke(ctx, s.fullName, req, resp)
	lat := time.Since(start)
	st, _ := status.FromError(err)
	code := st.Code()
	if err != nil && code == codes.Unknown && ctx.Err() == nil {
		kind := protocol.ClassifyErr(err)
		vu.Record(protocol.Result{Step: s.name, Latency: lat, Err: kind, TraceID: traceID})
		return protocol.Fail(s.name, kind, err.Error())
	}
	if !s.codes[code] {
		kind := "grpc:" + code.String()
		if code == codes.DeadlineExceeded {
			kind = "timeout"
		} else if code == codes.Canceled {
			kind = "canceled"
		}
		vu.Record(protocol.Result{Step: s.name, Latency: lat, Err: kind, TraceID: traceID})
		return protocol.Fail(s.name, kind, st.Message())
	}
	var body []byte
	if err == nil {
		body, _ = protojson.Marshal(resp)
	}
	r := &protocol.Response{Status: int(code), Body: body, Latency: lat}
	kind, msg := s.check.Verify(r, vu, false)
	if kind == "" {
		kind, msg = s.extract.Apply(r, vu.Vars)
	}
	vu.Last = r
	vu.Record(protocol.Result{Step: s.name, Latency: lat, Bytes: int64(proto.Size(resp)), Err: kind, TraceID: traceID})
	if kind != "" {
		return protocol.Fail(s.name, kind, msg)
	}
	return nil
}
