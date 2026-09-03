package broker

// skill_file_path_test.go — unit tests for validSkillFilePath, the shared
// relative-path safety rule for the two full-tree skill file trees
//.

import "testing"

func TestValidSkillFilePath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"references/notes.md", true},
		{"scripts/run.py", true},
		{"LICENSE.txt", true},
		{"", false},
		{"..", false},
		{"../escape.txt", false},
		{"references/../../escape.txt", false},
		{"/etc/passwd", false},
		{"a\\b", false},
		{"a\x00b", false},
		{"./notes.md", false},
		{"notes.md/", false},
		{"C:\\Users\\foo", false},
		{"C:/Users/foo", false},
		{"c:/x", false},
		{"D:/data", false},
	}
	for _, c := range cases {
		if got := validSkillFilePath(c.path); got != c.want {
			t.Errorf("validSkillFilePath(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}
