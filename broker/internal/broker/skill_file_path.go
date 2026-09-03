package broker

// skill_file_path.go — shared relative-path safety validator for the two
// full-tree skill file trees introduced by :
// admin agent-skill bundles' extras JSONB and personal skills'
// Skills/<name>/ workspace subtree. One rule, three call sites: admin bundle
// upsert (agent_skill_admin.go) and both file-read south RPCs
// (agent_skill_south.go, personal_skills_south.go).

import (
	"path"
	"strings"
)

// maxSkillBundleFiles bounds how many files an admin skill bundle's files map
// may carry.
const maxSkillBundleFiles = 100

// validSkillFilePath reports whether rel is safe to use as a skill-tree
// relative file path: non-empty, no absolute prefix, no ".." segment, no
// backslash, no NUL byte, no Windows drive letters, and unchanged by
// path.Clean (catches a leading "./", a trailing slash, and other non-canonical forms).
func validSkillFilePath(rel string) bool {
	if rel == "" {
		return false
	}
	if strings.ContainsRune(rel, '\\') || strings.ContainsRune(rel, 0) {
		return false
	}
	if path.IsAbs(rel) {
		return false
	}
	// Reject Windows drive paths like C: and C:/ (path.IsAbs is POSIX-only).
	if len(rel) >= 2 && rel[1] == ':' && isASCIILetter(rel[0]) {
		return false
	}
	for _, seg := range strings.Split(rel, "/") {
		if seg == ".." {
			return false
		}
	}
	return path.Clean(rel) == rel
}

// isASCIILetter reports whether b is an ASCII letter (A-Z or a-z).
func isASCIILetter(b byte) bool {
	return (b >= 'A' && b <= 'Z') || (b >= 'a' && b <= 'z')
}
