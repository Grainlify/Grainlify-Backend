package github

import "fmt"

// AvatarURL builds a GitHub avatar URL from a login. Always use
// avatars.githubusercontent.com (GitHub's dedicated avatar CDN), never
// github.com/{login}.png - that form redirects through the main site and is
// unreliable under concurrent embed traffic (a page rendering many avatars
// at once, e.g. a leaderboard, would intermittently fail to load some of
// them).
func AvatarURL(login string, size int) string {
	if size <= 0 {
		return fmt.Sprintf("https://avatars.githubusercontent.com/%s", login)
	}
	return fmt.Sprintf("https://avatars.githubusercontent.com/%s?s=%d", login, size)
}
