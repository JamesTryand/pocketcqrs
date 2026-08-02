package functions

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/dop251/goja"
)

func compile(t *testing.T, src string) *goja.Program {
	t.Helper()
	prog, err := goja.Compile("test.js", src, false)
	if err != nil {
		t.Fatal(err)
	}
	return prog
}

type fakeReader struct {
	record map[string]any
	rows   []map[string]any
}

func (f fakeReader) FindRecord(collection, id string) (map[string]any, error) {
	return f.record, nil
}
func (f fakeReader) Query(collection, filter string, limit int) ([]map[string]any, error) {
	return f.rows, nil
}

func TestRunHTTPShapedResponse(t *testing.T) {
	rt := NewGojaRuntime(nil)
	prog := compile(t, `function handle(request) {
		return {status: 201, body: {echo: request.body.name, q: request.query.x}, headers: {"X-Fn": "yes"}};
	}`)

	result, err := rt.runHTTP("test", prog, map[string]any{
		"method": "POST",
		"query":  map[string]any{"x": "1"},
		"body":   map[string]any{"name": "james"},
	})
	if err != nil {
		t.Fatal(err)
	}

	status, body, headers := shapeResponse(result)
	if status != 201 {
		t.Fatalf("expected 201, got %d", status)
	}
	m := body.(map[string]any)
	if m["echo"] != "james" || m["q"] != "1" {
		t.Fatalf("unexpected body: %v", m)
	}
	if headers["X-Fn"] != "yes" {
		t.Fatalf("unexpected headers: %v", headers)
	}
}

func TestRunHTTPPlainValueAndMissingHandle(t *testing.T) {
	rt := NewGojaRuntime(nil)

	result, err := rt.runHTTP("plain", compile(t, `function handle() { return {a: 1}; }`), map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	status, body, _ := shapeResponse(result)
	if status != 200 || body.(map[string]any)["a"].(int64) != 1 {
		t.Fatalf("got status=%d body=%v", status, body)
	}

	_, err = rt.runHTTP("nohandle", compile(t, `var x = 1;`), map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "handle") {
		t.Fatalf("expected handle error, got %v", err)
	}
}

func TestReaderBinding(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	rt := NewGojaRuntime(func(msg string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, msg)
	})
	rt.SetReader(fakeReader{
		record: map[string]any{"id": "r1", "title": "found"},
		rows:   []map[string]any{{"id": "r1"}, {"id": "r2"}},
	})

	prog := compile(t, `function handle(request) {
		var rec = pb.findRecord("tasks", "r1");
		var rows = pb.query("tasks", "", 10);
		return {title: rec.title, count: rows.length};
	}`)

	result, err := rt.runHTTP("reader", prog, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	m := result.(map[string]any)
	if m["title"] != "found" || m["count"].(int64) != 2 {
		t.Fatalf("unexpected result: %v", m)
	}
}

func TestReaderBindingInertWithoutReader(t *testing.T) {
	rt := NewGojaRuntime(nil)
	prog := compile(t, `function handle() {
		var rec = pb.findRecord("tasks", "r1");
		return {rec: rec};
	}`)
	result, err := rt.runHTTP("inert", prog, map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["rec"] != nil {
		t.Fatalf("expected nil record, got %v", result)
	}
}

func TestRunHTTPErrorPropagates(t *testing.T) {
	rt := NewGojaRuntime(nil)
	prog := compile(t, `function handle() { throw new Error("boom"); }`)
	_, err := rt.runHTTP("boom", prog, map[string]any{})
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected boom, got %v", err)
	}
	if errors.Is(err, nil) {
		t.Fatal("unreachable")
	}
}
