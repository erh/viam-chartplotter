//go:build delkey

// delkey: delete an API key by id via the Viam app API (CLI user token).
//
//	go run -tags delkey ./cmd/keycheck <key-id> [...]
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	apppb "go.viam.com/api/app/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func main() {
	home, _ := os.UserHomeDir()
	raw, err := os.ReadFile(filepath.Join(home, ".viam", "cached_cli_config.json"))
	if err != nil {
		panic(err)
	}
	var cfg struct {
		Auth struct {
			AccessToken string `json:"access_token"`
		} `json:"auth"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		panic(err)
	}
	conn, err := grpc.Dial("app.viam.com:443",
		grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{})))
	if err != nil {
		panic(err)
	}
	defer conn.Close()
	ctx := metadata.AppendToOutgoingContext(context.Background(),
		"authorization", "Bearer "+cfg.Auth.AccessToken)
	app := apppb.NewAppServiceClient(conn)
	for _, id := range os.Args[1:] {
		if _, err := app.DeleteKey(ctx, &apppb.DeleteKeyRequest{Id: id}); err != nil {
			fmt.Printf("delete %s: %v\n", id, err)
		} else {
			fmt.Printf("deleted %s\n", id)
		}
	}
}
