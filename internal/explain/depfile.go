package explain

import (
	"strings"
)

// ParseDepFile pulls the list of dependency paths out of a Make-style
// `.d` file as produced by `gcc -MMD` / `clang -MMD`. The format is:
//
//	target: dep1 dep2 \
//	    dep3 dep4
//
// We treat everything after the first ":" on the first non-blank
// line as the dependency list, joining continuation lines, splitting
// on whitespace, and dropping the target itself. Backslash-escaped
// spaces in paths (gcc emits these for paths with spaces) are
// resolved.
//
// Returns the dep paths in the order they appear in the file —
// callers that need a set should hash into a map.
//
// Robust to multi-target rules: only the first rule's deps are
// returned. In practice gcc / clang emit a single rule with the .o
// as the target; anything else is a corrupt or hand-written .d.
func ParseDepFile(content []byte) []string {
	// Strip line continuations: a backslash at end-of-line means
	// "this line continues into the next." Replace "\\\n" with " ".
	text := strings.ReplaceAll(string(content), "\\\n", " ")
	// Windows-style CRLF: do the same with "\\\r\n".
	text = strings.ReplaceAll(text, "\\\r\n", " ")

	// Take the first non-blank, non-comment line.
	var line string
	for _, l := range strings.Split(text, "\n") {
		trimmed := strings.TrimSpace(l)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		line = trimmed
		break
	}
	if line == "" {
		return nil
	}

	// "target: deps..." — chop at the first colon. Windows paths can
	// have a colon after a drive letter ("C:\foo.o: …"), so look
	// for the FIRST colon that is followed by whitespace (or end of
	// string), which is the rule separator.
	sep := -1
	for i := 0; i < len(line); i++ {
		if line[i] != ':' {
			continue
		}
		// End of string, space, tab, or another colon → real
		// separator.
		if i == len(line)-1 || line[i+1] == ' ' || line[i+1] == '\t' {
			sep = i
			break
		}
	}
	if sep < 0 {
		return nil
	}
	rest := line[sep+1:]

	// Split on whitespace, handling backslash-escaped spaces inline.
	deps := splitDepTokens(rest)
	return deps
}

// splitDepTokens tokenises a dep-list segment on whitespace, honoring
// "\ " as an in-token escaped space (the form gcc emits for paths
// with spaces). Empty tokens are skipped.
func splitDepTokens(s string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '\\' && i+1 < len(s) && s[i+1] == ' ' {
			cur.WriteByte(' ')
			i++
			continue
		}
		if c == ' ' || c == '\t' {
			if cur.Len() > 0 {
				out = append(out, cur.String())
				cur.Reset()
			}
			continue
		}
		cur.WriteByte(c)
	}
	if cur.Len() > 0 {
		out = append(out, cur.String())
	}
	return out
}
