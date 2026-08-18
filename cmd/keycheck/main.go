//go:build !dialtest && !delkey

// keycheck: debugging aid — list a machine's API keys via the Viam app API,
// authenticating with the CLI's cached user token. Not part of the product.
//
//	go run ./cmd/keycheck <robot-id>
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
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: keycheck <robot-id>")
		os.Exit(1)
	}
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
	client := apppb.NewAppServiceClient(conn)
	resp, err := client.GetRobotAPIKeys(ctx, &apppb.GetRobotAPIKeysRequest{RobotId: os.Args[1]})
	if err != nil {
		panic(err)
	}
	for _, k := range resp.ApiKeys {
		fmt.Printf("id=%s name=%q key=%s created=%s\n",
			k.ApiKey.Id, k.ApiKey.Name, k.ApiKey.Key, k.ApiKey.CreatedOn.AsTime())
		for _, a := range k.Authorizations {
			fmt.Printf("  auth: type=%s id=%s resourceType=%s resourceId=%s org=%s\n",
				a.AuthorizationType, a.AuthorizationId, a.ResourceType, a.ResourceId, a.OrgId)
		}
	}
}
