package rdsiam

import (
	"context"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/jackc/pgx/v5"
)

const rdsHost = "slagworks.abcdefghijkl.us-east-2.rds.amazonaws.com"

var testAWS = &aws.Config{Region: "us-west-2", Credentials: credentials.NewStaticCredentialsProvider("AKIDEXAMPLE", "secret", "")}

func parse(t *testing.T, dsn string) *pgx.ConnConfig {
	t.Helper()
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestPasswordAndLocalDSNsAreLeftAlone(t *testing.T) {
	for _, dsn := range []string{
		"host=localhost user=slagworks password=slagworks dbname=slagworks sslmode=disable",
		"postgres://slagworks_pw:secret@" + rdsHost + ":5432/slagworks?sslmode=require",
	} {
		hook, err := Configure(context.Background(), parse(t, dsn), testAWS)
		if err != nil || hook != nil {
			t.Errorf("%s: hook=%v err=%v, want a plain login", dsn, hook != nil, err)
		}
	}
}

// TestIAMRefusesUnverifiedTLS: verify-ca checks the chain but not the host name (pgx
// skips Go's verification and runs its own), so it's refused like the rest.
func TestIAMRefusesUnverifiedTLS(t *testing.T) {
	for _, mode := range []string{"require", "prefer", "verify-ca", "disable"} {
		_, err := Configure(context.Background(), parse(t, "postgres://slagworks_iam@"+rdsHost+":5432/slagworks?sslmode="+mode), testAWS)
		if err == nil || !strings.Contains(err.Error(), "verify-full") {
			t.Errorf("sslmode=%s: err=%v, want a refusal naming verify-full", mode, err)
		}
	}
}

func TestIAMSetsATokenAndTheRDSRoots(t *testing.T) {
	cfg := parse(t, "postgres://slagworks_iam@"+rdsHost+":5432/slagworks?sslmode=verify-full")
	hook, err := Configure(context.Background(), cfg, testAWS)
	if err != nil || hook == nil {
		t.Fatalf("hook=%v err=%v, want an IAM login", hook != nil, err)
	}
	if cfg.TLSConfig.RootCAs == nil || cfg.TLSConfig.ServerName != rdsHost {
		t.Fatalf("TLS not verifying %s against the RDS roots", rdsHost)
	}
	cc := cfg.Copy()
	if err := hook(context.Background(), cc); err != nil {
		t.Fatal(err)
	}
	// The token is a presigned URL for this endpoint and user, signed for the
	// endpoint's region (us-east-2), not the AWS config's.
	for _, want := range []string{rdsHost + ":5432?", "Action=connect", "DBUser=slagworks_iam", "us-east-2%2Frds-db"} {
		if !strings.Contains(cc.Password, want) {
			t.Errorf("token %q lacks %q", cc.Password, want)
		}
	}
	if cfg.Password != "" {
		t.Error("the hook changed the shared config, not the connection's copy")
	}
}

func TestEmbeddedBundleHasTheRDSRoots(t *testing.T) {
	if n := strings.Count(string(rdsCABundle), "BEGIN CERTIFICATE"); n < 50 {
		t.Errorf("embedded RDS bundle has %d certificates, want the global bundle (100+)", n)
	}
}
