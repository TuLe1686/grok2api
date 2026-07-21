package account

import (
	"net/http"
	"testing"
	"time"

	accountdomain "github.com/chenyme/grok2api/backend/internal/domain/account"
)

func TestIsBuildChatPermissionDenied(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{
			name:   "top-level code and message",
			status: http.StatusForbidden,
			body:   `{"code":"permission-denied","error":"Access to the chat endpoint is denied. Please ensure you're using the correct credentials."}`,
			want:   true,
		},
		{
			name:   "nested error object",
			status: http.StatusForbidden,
			body:   `{"error":{"code":"permission_denied","message":"Access to the chat endpoint is denied"}}`,
			want:   true,
		},
		{
			name:   "generic forbidden",
			status: http.StatusForbidden,
			body:   `{"error":"upstream policy rejected request"}`,
			want:   false,
		},
		{
			name:   "not forbidden",
			status: http.StatusUnauthorized,
			body:   `{"code":"permission-denied","error":"Access to the chat endpoint is denied"}`,
			want:   false,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if got := IsBuildChatPermissionDenied(test.status, []byte(test.body)); got != test.want {
				t.Fatalf("got %v, want %v", got, test.want)
			}
		})
	}
}

func TestDisableBuildChatPermissionDeniedDisablesBuildOnly(t *testing.T) {
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	service, repo := newAutoCleanTestService(t, now)
	build := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderBuild, Name: "build-denied", SourceKey: "build-denied",
		EncryptedAccessToken: "token", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err := service.DisableBuildChatPermissionDenied(t.Context(), build, buildChatPermissionDeniedReason); err != nil {
		t.Fatal(err)
	}
	updated, err := repo.Get(t.Context(), build.ID)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Enabled || updated.LastError == "" {
		t.Fatalf("build account was not disabled: %#v", updated)
	}

	web := mustUpsert(t, repo, accountdomain.Credential{
		Provider: accountdomain.ProviderWeb, AuthType: accountdomain.AuthTypeSSO, Name: "web", SourceKey: "web",
		EncryptedAccessToken: "web-token", Enabled: true, AuthStatus: accountdomain.AuthStatusActive,
	})
	if err := service.DisableBuildChatPermissionDenied(t.Context(), web, buildChatPermissionDeniedReason); err != nil {
		t.Fatal(err)
	}
	webAfter, err := repo.Get(t.Context(), web.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !webAfter.Enabled {
		t.Fatalf("web account was disabled: %#v", webAfter)
	}
}

func TestRequestDisableBuildChatPermissionDeniedDefault(t *testing.T) {
	service, _ := newAutoCleanTestService(t, time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC))
	if !service.RequestDisableBuildChatPermissionDenied() {
		t.Fatal("request disable should default to true")
	}
	service.UpdateBuildChatPermissionDeniedConfig(BuildChatPermissionDeniedConfig{
		RequestDisable: false, InspectEnabled: false, InspectInterval: time.Hour, InspectConcurrency: 2,
	})
	if service.RequestDisableBuildChatPermissionDenied() {
		t.Fatal("request disable should honor config updates")
	}
}
