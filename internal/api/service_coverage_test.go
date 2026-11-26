package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/ALT-F4-LLC/docket/internal/api/docketv1"
	"github.com/ALT-F4-LLC/docket/internal/api/docketv1/docketv1connect"
)

// TestEveryMethodImplemented calls each DocketService method the schema
// declares and fails when one answers unimplemented, the answer of the
// embedded UnimplementedDocketServiceHandler stub. The method set comes from
// the generated descriptor, so a method added to the schema without a
// Service method fails here.
//
// Each call sends the zero request: its binary wire form is empty for every
// message type, so emptypb.Empty stands in for each method's input. The
// generated handler is mounted without the daemon's interceptors, so request
// validation cannot answer before the method does.
func TestEveryMethodImplemented(t *testing.T) {
	path, handler := docketv1connect.NewDocketServiceHandler(NewService(openAPIStore(t), "dev"))
	mux := http.NewServeMux()
	mux.Handle(path, handler)
	server := httptest.NewServer(mux)
	t.Cleanup(server.Close)

	service := docketv1.File_docket_v1_docket_proto.Services().ByName("DocketService")
	if service == nil {
		t.Fatal("descriptor declares no DocketService")
	}
	methods := service.Methods()
	if methods.Len() == 0 {
		t.Fatal("DocketService descriptor declares no methods")
	}
	for i := range methods.Len() {
		method := methods.Get(i)
		name := string(method.Name())
		t.Run(name, func(t *testing.T) {
			procedure := "/" + string(service.FullName()) + "/" + name
			c := connect.NewClient[emptypb.Empty, emptypb.Empty](server.Client(), server.URL+procedure)

			_, err := c.CallUnary(context.Background(), connect.NewRequest(&emptypb.Empty{}))

			if connect.CodeOf(err) == connect.CodeUnimplemented {
				t.Errorf("%s answered unimplemented: no *Service method serves it (error: %v)", procedure, err)
			}
		})
	}
}
