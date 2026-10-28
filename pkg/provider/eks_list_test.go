package provider

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/eks"
)

// fakeEKSProvider points an EKSProvider at a fake EKS REST endpoint. Cluster
// names in denied get an AccessDeniedException from DescribeCluster.
func fakeEKSProvider(t *testing.T, names []string, denied map[string]bool) *EKSProvider {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == "/clusters" {
			_ = json.NewEncoder(w).Encode(map[string]any{"clusters": names})
			return
		}
		name := strings.TrimPrefix(r.URL.Path, "/clusters/")
		if denied[name] {
			w.Header().Set("X-Amzn-ErrorType", "AccessDeniedException")
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"message":"not authorized to describe ` + name + `"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cluster": map[string]any{
			"name": name, "status": "ACTIVE", "arn": "arn:aws:eks:us-east-1:1:cluster/" + name,
		}})
	}))
	t.Cleanup(srv.Close)

	client := eks.New(eks.Options{
		Region:           "us-east-1",
		BaseEndpoint:     aws.String(srv.URL),
		Credentials:      credentials.NewStaticCredentialsProvider("AK", "SK", ""),
		RetryMaxAttempts: 1,
	})
	return &EKSProvider{config: EKSConfig{ProfileID: "p1", ProfileName: "prof", Region: "us-east-1"}, eksClient: client}
}

func TestEKSListClusters_PartialDescribeFailureKeepsCluster(t *testing.T) {
	p := fakeEKSProvider(t, []string{"b-denied", "a-ok"}, map[string]bool{"b-denied": true})

	clusters, err := p.ListClusters()
	if err != nil {
		t.Fatalf("partial failure must not fail the list: %v", err)
	}
	if len(clusters) != 2 {
		t.Fatalf("clusters = %+v, want 2", clusters)
	}
	if clusters[0].Name != "a-ok" || clusters[0].State != "ACTIVE" {
		t.Errorf("first cluster = %+v", clusters[0])
	}
	d := clusters[1]
	if d.ID != "b-denied" || d.State != "unknown" || d.Provider != "eks" || d.ProfileID != "p1" {
		t.Errorf("denied cluster = %+v", d)
	}
	if !strings.Contains(d.Description, "describe failed") || !strings.Contains(d.Description, "AccessDenied") {
		t.Errorf("description = %q, want the describe error", d.Description)
	}
}

func TestEKSListClusters_AllDescribeFailuresIsError(t *testing.T) {
	p := fakeEKSProvider(t, []string{"x", "y"}, map[string]bool{"x": true, "y": true})
	if _, err := p.ListClusters(); err == nil || !strings.Contains(err.Error(), "failed to describe any EKS cluster") {
		t.Errorf("error = %v", err)
	}
}

func TestEKSGenerateKubeconfig_ProfileOnlyHasNoEnv(t *testing.T) {
	p := &EKSProvider{config: EKSConfig{Region: "us-east-1", AWSProfile: "dev"}}
	exec := loadGenerated(t, p, "c3").AuthInfos["c3"].Exec
	if exec.Args[0] != "--profile" || exec.Args[1] != "dev" || len(exec.Env) != 0 {
		t.Errorf("args = %v env = %v, want --profile dev and no env", exec.Args, exec.Env)
	}
}
