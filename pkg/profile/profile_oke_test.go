package profile

import "testing"

func TestValidateOKE(t *testing.T) {
	ok := &Profile{Name: "oci", Type: ProfileTypeOKE}
	if err := ok.Validate(); err != nil {
		t.Fatalf("minimal OKE profile should validate: %v", err)
	}
	full := &Profile{Name: "oci", Type: ProfileTypeOKE, OCIProfile: "kasm", OCIRegion: "us-ashburn-1",
		OCICompartment: "ocid1.compartment.oc1..aaaa", OCIAuth: OCIAuthSecurityToken, OKEEndpoint: OKEEndpointPrivate}
	if err := full.Validate(); err != nil {
		t.Fatalf("full OKE profile should validate: %v", err)
	}
	for _, bad := range []*Profile{
		{Name: "x", Type: ProfileTypeOKE, OCIAuth: "password"},
		{Name: "x", Type: ProfileTypeOKE, OKEEndpoint: "vpn"},
		{Name: "x", Type: ProfileTypeOKE, OCICompartment: "root"},
	} {
		if err := bad.Validate(); err == nil {
			t.Errorf("expected validation error for %+v", bad)
		}
	}
	req := &ProfileCreateRequest{Name: "n", Type: ProfileTypeOKE, OCIProfile: " kasm ", OCICompartment: " ocid1.c "}
	p := req.ToProfile("id")
	if p.OCIProfile != "kasm" || p.OCICompartment != "ocid1.c" {
		t.Errorf("ToProfile should trim OKE fields: %+v", p)
	}
	v := p.View()
	if v.OCIProfile != "kasm" || v.Type != ProfileTypeOKE {
		t.Errorf("View should carry OKE fields: %+v", v)
	}
	stray := &ProfileCreateRequest{Name: "n", Type: ProfileTypeOKE, Token: "t", Password: "p", SecretKey: "s", Kubeconfig: "k"}
	if sp := stray.ToProfile("id"); hasSecrets(sp) {
		t.Errorf("OKE profile must not store secrets: %+v", sp)
	}
}
