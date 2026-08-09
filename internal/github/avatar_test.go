package github

import "testing"

func TestAvatarURL(t *testing.T) {
	tests := []struct {
		name  string
		login string
		size  int
		want  string
	}{
		{"with size", "octocat", 200, "https://avatars.githubusercontent.com/octocat?s=200"},
		{"zero size omits query", "octocat", 0, "https://avatars.githubusercontent.com/octocat"},
		{"negative size omits query", "octocat", -1, "https://avatars.githubusercontent.com/octocat"},
		{"org login", "stellopay", 80, "https://avatars.githubusercontent.com/stellopay?s=80"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AvatarURL(tt.login, tt.size); got != tt.want {
				t.Errorf("AvatarURL(%q, %d) = %q, want %q", tt.login, tt.size, got, tt.want)
			}
		})
	}
}
