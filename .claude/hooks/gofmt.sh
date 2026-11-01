#!/bin/bash
# PostToolUse (Edit|Write): gofmt the Go file that was just written.
file=$(jq -r '.tool_input.file_path // empty')
case "$file" in
  *.go) [ -f "$file" ] && gofmt -w "$file" 2>&1 >/dev/null ;;
esac
exit 0
