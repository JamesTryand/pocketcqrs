package functions

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"github.com/dop251/goja"
	"github.com/pocketbase/pocketbase/apis"
	"github.com/pocketbase/pocketbase/core"
)

// HTTPRegistry holds the HTTP-triggered functions.
type HTTPRegistry struct {
	runtime *GojaRuntime
	fns     map[string]*goja.Program
}

// NewHTTPRegistry creates an empty registry bound to runtime.
func NewHTTPRegistry(runtime *GojaRuntime) *HTTPRegistry {
	return &HTTPRegistry{runtime: runtime, fns: map[string]*goja.Program{}}
}

func (r *HTTPRegistry) register(name string, prog *goja.Program) {
	r.fns[name] = prog
}

// Names returns the registered function names (their /api/fn/ paths).
func (r *HTTPRegistry) Names() []string {
	out := make([]string, 0, len(r.fns))
	for name := range r.fns {
		out = append(out, name)
	}
	return out
}

// RegisterHTTPRoutes binds:
//
//	POST|GET /api/fn/{name}
//
// The function must define handle(request); its return value becomes the
// response: an object {status, body, headers} is honored as-is, anything
// else is returned as the JSON body with 200. requireAuth applies
// PocketBase's standard auth middleware (the auth record, if any, is
// exposed to the function as request.auth).
func RegisterHTTPRoutes(e *core.ServeEvent, reg *HTTPRegistry, requireAuth bool) {
	handler := func(re *core.RequestEvent) error {
		name := re.Request.PathValue("name")
		prog, ok := reg.fns[name]
		if !ok {
			return apis.NewNotFoundError("unknown function: "+name, nil)
		}

		req, err := buildRequest(re)
		if err != nil {
			return apis.NewBadRequestError("failed reading request body", err)
		}

		result, err := reg.runtime.runHTTP(name, prog, req)
		if err != nil {
			reg.runtime.logger("function failed", "function", name, "error", err)
			return re.JSON(http.StatusInternalServerError, map[string]string{"error": err.Error()})
		}

		status, body, headers := shapeResponse(result)
		for k, v := range headers {
			re.Response.Header().Set(k, v)
		}
		return re.JSON(status, body)
	}

	post := e.Router.POST("/api/fn/{name}", handler)
	get := e.Router.GET("/api/fn/{name}", handler)
	if requireAuth {
		post.Bind(apis.RequireAuth())
		get.Bind(apis.RequireAuth())
	}
}

// buildRequest maps the incoming HTTP request to the JS `request` object.
func buildRequest(re *core.RequestEvent) (map[string]any, error) {
	var bodyText string
	if re.Request.Body != nil {
		raw, err := io.ReadAll(re.Request.Body)
		if err != nil {
			return nil, err
		}
		bodyText = string(raw)
	}
	var bodyJSON any
	if bodyText != "" {
		if err := json.Unmarshal([]byte(bodyText), &bodyJSON); err != nil {
			bodyJSON = nil // not JSON; exposed as request.bodyText only
		}
	}

	query := map[string]any{}
	for k, vs := range re.Request.URL.Query() {
		if len(vs) == 1 {
			query[k] = vs[0]
		} else {
			query[k] = vs
		}
	}

	headers := map[string]string{}
	for k := range re.Request.Header {
		headers[k] = re.Request.Header.Get(k)
	}

	var auth any
	if re.Auth != nil {
		auth = map[string]any{
			"id":         re.Auth.Id,
			"collection": re.Auth.Collection().Name,
		}
	}

	return map[string]any{
		"method":   re.Request.Method,
		"path":     re.Request.URL.Path,
		"query":    query,
		"headers":  headers,
		"body":     bodyJSON,
		"bodyText": bodyText,
		"auth":     auth,
	}, nil
}

// runHTTP executes the script and calls its handle(request) function.
func (r *GojaRuntime) runHTTP(name string, prog *goja.Program, req map[string]any) (result any, err error) {
	defer func() {
		if rec := recover(); rec != nil {
			result = nil
			err = fmt.Errorf("%v", rec)
		}
	}()

	vm, timer := r.newVM(name)
	defer timer.Stop()

	vm.Set("request", req)
	if _, err := vm.RunProgram(prog); err != nil {
		return nil, err
	}

	handle, ok := goja.AssertFunction(vm.Get("handle"))
	if !ok {
		return nil, fmt.Errorf("function %s does not define handle(request)", name)
	}

	v, err := handle(goja.Undefined(), vm.ToValue(req))
	if err != nil {
		return nil, err
	}
	return v.Export(), nil
}

// shapeResponse interprets the handle() return value.
func shapeResponse(result any) (status int, body any, headers map[string]string) {
	status = http.StatusOK
	body = result
	headers = map[string]string{}

	m, ok := result.(map[string]any)
	if !ok {
		return status, body, headers
	}

	if s, ok := m["status"]; ok {
		switch v := s.(type) {
		case int64:
			status = int(v)
		case float64:
			status = int(v)
		case int:
			status = v
		}
	}
	if b, ok := m["body"]; ok {
		body = b
	}
	if h, ok := m["headers"].(map[string]any); ok {
		for k, v := range h {
			headers[k] = fmt.Sprint(v)
		}
	}
	return status, body, headers
}
