package convert

import (
	"strings"
	"testing"
)

const petstore = `
openapi: 3.1.0
info: { title: Pets }
servers:
  - url: https://{env}.pets.test/v1
    variables: { env: { default: api } }
security: [{ key: [] }]
components:
  securitySchemes:
    key: { type: apiKey, in: header, name: X-Api-Key }
  parameters:
    PetId: { name: pet-id, in: path, required: true, schema: { type: string, format: uuid } }
  schemas:
    Pet:
      type: object
      properties:
        id: { type: integer, readOnly: true }
        name: { type: string }
        kind: { type: string, enum: [cat, dog] }
        born: { type: string, format: date-time }
        tags: { type: array, items: { type: string } }
        weight: { type: number, minimum: 1.5 }
        owner: { type: [object, "null"], properties: { email: { type: string, format: email } } }
        parent: { $ref: "#/components/schemas/Pet" }
    NewPet:
      allOf:
        - $ref: "#/components/schemas/Pet"
        - type: object
          properties: { vaccinated: { type: boolean } }
paths:
  /pets/{pet-id}:
    parameters: [{ $ref: "#/components/parameters/PetId" }]
    put:
      requestBody:
        content:
          application/json: { schema: { $ref: "#/components/schemas/NewPet" } }
      responses: { "2XX": { description: ok } }
    get:
      operationId: getPet
      parameters:
        - { name: fields, in: query, required: true, schema: { type: string }, example: all }
        - { name: verbose, in: query, schema: { type: boolean } }
        - { name: X-Req, in: header, required: true, schema: { type: integer } }
      responses: { "404": { description: no }, "200": { description: ok } }
  /pets:
    post:
      requestBody:
        content:
          application/x-www-form-urlencoded:
            schema: { type: object, properties: { name: { type: string }, age: { type: integer } } }
      responses: { "201": { description: ok } }
    delete:
      deprecated: true
      responses: { "204": { description: ok } }
  /upload:
    post:
      security: []
      requestBody:
        content:
          text/plain: { example: "hello" }
      responses: { default: { description: ok } }
`

func TestOpenAPI(t *testing.T) {
	y, f := convertOK(t, FormatOpenAPI, petstore, Options{})
	eq(t, f.Vars["baseUrl"], "https://api.pets.test/v1")
	eq(t, f.Vars["pet_id"], "00000000-0000-0000-0000-000000000000")
	eq(t, f.Vars["key"], "changeme")
	for _, want := range []string{"4 operation(s)", "skipped 1 deprecated", "#   GET /pets/{pet-id}", "# PUT /pets/{pet-id}"} {
		if !strings.Contains(y, want) {
			t.Errorf("missing %q in:\n%s", want, y)
		}
	}
	hs := flowHTTP(t, f)
	if len(hs) != 4 {
		t.Fatalf("want 4 steps:\n%s", y)
	}
	// Spec order for paths, fixed method order within a path.
	get, put, post, upload := hs[0], hs[1], hs[2], hs[3]
	eq(t, get["name"], "getPet")
	eq(t, get["url"], "{{ baseUrl }}/pets/{{ pet_id }}")
	eq(t, get["query"], map[string]any{"fields": "all"})
	eq(t, get["headers"], map[string]any{"X-Req": "0", "X-Api-Key": "{{ key }}"})
	eq(t, get["check"], map[string]any{"status": 200})

	eq(t, put["method"], "PUT")
	eq(t, put["check"], map[string]any{"status": "2xx"})
	pet := put["json"].(map[string]any)
	if _, ok := pet["id"]; ok {
		t.Error("readOnly property included")
	}
	eq(t, pet["kind"], "cat")
	eq(t, pet["born"], "2024-01-01T00:00:00Z")
	eq(t, pet["tags"], []any{"string"})
	eq(t, pet["weight"], 1.5)
	eq(t, pet["vaccinated"], true)
	eq(t, pet["owner"], map[string]any{"email": "user@example.com"})
	if _, ok := pet["parent"]; ok {
		t.Error("recursive property should be left out")
	}

	eq(t, post["form"], map[string]any{"name": "string", "age": 0})
	eq(t, post["check"], map[string]any{"status": 201})

	eq(t, upload["body"], "hello")
	eq(t, upload["headers"], map[string]any{"Content-Type": "text/plain"})
	eq(t, upload["check"], nil)

	_, f = convertOK(t, FormatOpenAPI, petstore, Options{All: true, BaseURL: "http://localhost:3000"})
	eq(t, len(flowHTTP(t, f)), 5)
	eq(t, f.Vars["baseUrl"], "http://localhost:3000")
}

func TestSwagger2(t *testing.T) {
	const sw = `{
  "swagger": "2.0",
  "host": "api.test", "basePath": "/v2", "schemes": ["http"],
  "securityDefinitions": { "bearer": { "type": "apiKey", "in": "header", "name": "Authorization" } },
  "definitions": { "User": { "type": "object", "required": ["name"], "properties": { "name": { "type": "string", "example": "bob" } } } },
  "paths": {
    "/users/{id}": {
      "get": { "parameters": [{ "name": "id", "in": "path", "required": true, "type": "integer" }],
               "responses": { "200": { "description": "ok" } } }
    },
    "/users": {
      "post": { "security": [{ "bearer": [] }],
                "parameters": [{ "name": "body", "in": "body", "schema": { "$ref": "#/definitions/User" } }],
                "responses": { "201": { "description": "ok" } } }
    }
  }
}`
	y, f := convertOK(t, "", sw, Options{})
	eq(t, f.Vars["baseUrl"], "http://api.test/v2")
	eq(t, f.Vars["id"], 1)
	hs := flowHTTP(t, f)
	if len(hs) != 2 {
		t.Fatalf("want 2 steps:\n%s", y)
	}
	eq(t, hs[0]["url"], "{{ baseUrl }}/users/{{ id }}")
	eq(t, hs[1]["json"], map[string]any{"name": "bob"})
	eq(t, hs[1]["headers"], map[string]any{"Authorization": "{{ bearer }}"})
}

func TestOpenAPIErrors(t *testing.T) {
	for _, in := range []string{"foo: bar", "openapi: 3.0.0\n", "openapi: 3.0.0\npaths:\n  /a:\n    get: { deprecated: true }\n", "{"} {
		if _, err := OpenAPI([]byte(in), Options{}); err == nil {
			t.Errorf("%q: expected error", in)
		}
	}
}

func TestOpenAPINoServers(t *testing.T) {
	y, f := convertOK(t, FormatOpenAPI, "openapi: 3.0.0\npaths:\n  /a:\n    get: {}\n", Options{})
	eq(t, f.Vars["baseUrl"], "http://localhost:8080")
	if !strings.Contains(y, "baseUrl is a guess") {
		t.Errorf("missing note:\n%s", y)
	}
}
