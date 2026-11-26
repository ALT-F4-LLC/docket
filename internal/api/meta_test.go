package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

func serve(t *testing.T, svc docketv1connect.DocketServiceHandler) *httptest.Server {
	t.Helper()
	path, handler := NewHandler(svc, discardLogger)
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)
	return server
}
