//go:build dialtest && !delkey

// dialtest: mint a fresh machine API key and immediately dial the robot with
// it, retrying until it works — measures authorization propagation delay.
//
//	go run -tags dialtest ./cmd/keycheck <robot-id> <org-id> <fqdn>
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	apppb "go.viam.com/api/app/v1"
	"go.viam.com/rdk/logging"
	"go.viam.com/rdk/robot/client"
	"go.viam.com/utils/rpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
)

func main() {
	if len(os.Args) < 4 {
		fmt.Fprintln(os.Stderr, "usage: dialtest <robot-id> <org-id> <fqdn>")
		os.Exit(1)
	}
	robotID, orgID, fqdn := os.Args[1], os.Args[2], os.Args[3]

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

	created := time.Now()
	var keyID, keyValue string
	deleteAfter := false
	if len(os.Args) >= 6 {
		// Existing key supplied: dial with it instead of minting.
		keyID, keyValue = os.Args[4], os.Args[5]
		fmt.Printf("using existing key id=%s\n", keyID)
	} else {
		resp, err := app.CreateKey(ctx, &apppb.CreateKeyRequest{
			Name: "dialtest-propagation",
			Authorizations: []*apppb.Authorization{{
				AuthorizationType: "role",
				AuthorizationId:   "robot_owner",
				ResourceType:      "robot",
				ResourceId:        robotID,
				IdentityId:        "",
				OrganizationId:    orgID,
				IdentityType:      "api-key",
			}},
		})
		if err != nil {
			panic(err)
		}
		keyID, keyValue = resp.Id, resp.Key
		deleteAfter = true
		fmt.Printf("key minted id=%s at t=0\n", keyID)
	}

	logger := logging.NewLogger("dialtest")
	for attempt := 1; ; attempt++ {
		dctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		start := time.Now()
		rc, err := client.New(dctx, fqdn, logger,
			client.WithDialOptions(rpc.WithEntityCredentials(keyID, rpc.Credentials{
				Type:    rpc.CredentialsTypeAPIKey,
				Payload: keyValue,
			})))
		cancel()
		since := time.Since(created).Round(time.Millisecond)
		if err == nil {
			fmt.Printf("attempt %d SUCCEEDED in %s (t+%s after mint)\n",
				attempt, time.Since(start).Round(time.Millisecond), since)
			rc.Close(context.Background())
			if deleteAfter {
				if _, err := app.DeleteKey(ctx, &apppb.DeleteKeyRequest{Id: keyID}); err == nil {
					fmt.Println("throwaway key deleted")
				}
			}
			return
		}
		fmt.Printf("attempt %d failed after %s (t+%s after mint): %v\n",
			attempt, time.Since(start).Round(time.Millisecond), since, err)
		if since > 2*time.Minute {
			fmt.Println("giving up after 2 minutes")
			os.Exit(1)
		}
		time.Sleep(3 * time.Second)
	}
}
