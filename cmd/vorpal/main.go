package main

import (
	"fmt"
	"log"

	"github.com/ALT-F4-LLC/vorpal/sdk/go/pkg/artifact"
	"github.com/ALT-F4-LLC/vorpal/sdk/go/pkg/artifact/language"
	"github.com/ALT-F4-LLC/vorpal/sdk/go/pkg/config"
)

func main() {
	ctx := config.GetContext()
	ctxTarget := ctx.GetTarget()

	systems := []string{
		"aarch64-darwin",
		"aarch64-linux",
		"x86_64-darwin",
		"x86_64-linux",
	}

	ffmpeg, err := ctx.FetchArtifactAlias("ffmpeg:8.0.1")
	if err != nil {
		log.Fatalf("failed to get ffmpeg: %v", err)
	}

	gobin, err := artifact.GoBin(ctx)
	if err != nil {
		log.Fatalf("failed to get go: %v", err)
	}

	goimports, err := artifact.Goimports(ctx)
	if err != nil {
		log.Fatalf("failed to get goimports: %v", err)
	}

	gopls, err := artifact.Gopls(ctx)
	if err != nil {
		log.Fatalf("failed to get gopls: %v", err)
	}

	protoc, err := artifact.Protoc(ctx)
	if err != nil {
		log.Fatalf("failed to get protoc: %v", err)
	}

	protocGenGo, err := artifact.ProtocGenGo(ctx)
	if err != nil {
		log.Fatalf("failed to get protoc-gen-go: %v", err)
	}

	protocGenGoGRPC, err := artifact.ProtocGenGoGRPC(ctx)
	if err != nil {
		log.Fatalf("failed to get protoc-gen-go-grpc: %v", err)
	}

	staticcheck, err := artifact.Staticcheck(ctx)
	if err != nil {
		log.Fatalf("failed to get staticcheck: %v", err)
	}

	ttyd, err := ctx.FetchArtifactAlias("ttyd:1.7.7")
	if err != nil {
		log.Fatalf("failed to get ttyd: %v", err)
	}

	vhs, err := ctx.FetchArtifactAlias("vhs:0.10.0")
	if err != nil {
		log.Fatalf("failed to get vhs: %v", err)
	}

	goarch, err := language.GetGOARCH(ctxTarget)
	if err != nil {
		log.Fatalf("failed to get GOARCH for target %s: %v", ctxTarget, err)
	}

	goos, err := language.GetGOOS(ctxTarget)
	if err != nil {
		log.Fatalf("failed to get GOOS for target %s: %v", ctxTarget, err)
	}

	_, err = artifact.
		NewDevelopmentEnvironment("docket-shell", systems).
		WithArtifacts([]*string{
			ffmpeg,
			gobin,
			goimports,
			gopls,
			protoc,
			protocGenGo,
			protocGenGoGRPC,
			staticcheck,
			ttyd,
			vhs,
		}).
		WithEnvironments([]string{
			"CGO_ENABLED=0",
			fmt.Sprintf("GOARCH=%s", *goarch),
			fmt.Sprintf("GOOS=%s", *goos),
		}).
		Build(ctx)
	if err != nil {
		log.Fatalf("error building project environment: %v", err)
	}

	_, err = language.NewGo("docket", systems).
		WithBuildDirectory("cmd/docket").
		WithIncludes([]string{
			"cmd/docket",
			"internal",
			"go.mod",
			"go.sum",
		}).
		Build(ctx)
	if err != nil {
		log.Fatalf("error building: %v", err)
	}

	ctx.Run()
}
