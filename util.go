package main

import (
	"fmt"
	"sort"
	"strings"
)

// truncate caps s at max bytes, appending a marker when it trims.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "\n...(diff truncated)..."
}

// truncateDiff caps a unified diff at max bytes while keeping every changed file
// visible. A plain truncate() head-cuts the diff, which silently drops whole
// files: `git diff` orders files by path, so a large change to an early-sorting
// name (e.g. uppercase README.md) can push every other file past the limit and
// leave the model describing only that one file. Instead we split per file and
// give each file a fair share of the budget, so none disappears entirely.
func truncateDiff(diff string, max int) string {
	if len(diff) <= max {
		return diff
	}
	files := splitDiffByFile(diff)
	if len(files) <= 1 {
		return truncate(diff, max)
	}

	// Fair-share allocation: files that already fit under an even share keep
	// their full diff; the budget they leave behind is redistributed among the
	// files still over the share, so a small file never wastes its slice.
	settled := make([]bool, len(files))
	remaining, unsettled := max, len(files)
	for unsettled > 0 {
		share := remaining / unsettled
		progress := false
		for i, f := range files {
			if settled[i] || len(f) > share {
				continue
			}
			settled[i] = true
			remaining -= len(f)
			unsettled--
			progress = true
		}
		if !progress {
			break
		}
	}

	share := max
	if unsettled > 0 {
		share = remaining / unsettled
	}
	var b strings.Builder
	for i, f := range files {
		if settled[i] {
			b.WriteString(f)
			continue
		}
		b.WriteString(truncate(f, share))
		b.WriteString("\n")
	}
	return b.String()
}

// chunkDiff splits a diff into a sequence of chunks, each no larger than max
// bytes, packing whole files together where they fit and splitting any single
// file that is itself too big. Unlike truncateDiff it drops nothing: the chunks
// together contain the entire diff, to be summarized part by part.
func chunkDiff(diff string, max int) []string {
	files := splitDiffByFile(diff)
	// splitDiffByFile drops the "\n" that separated each file; restore it so
	// packed files stay on their own lines and the chunks reconstruct the diff.
	for i, f := range files {
		if !strings.HasSuffix(f, "\n") {
			files[i] = f + "\n"
		}
	}
	var chunks []string
	var cur strings.Builder
	flush := func() {
		if cur.Len() > 0 {
			chunks = append(chunks, cur.String())
			cur.Reset()
		}
	}
	for _, f := range files {
		if len(f) > max {
			flush()
			chunks = append(chunks, splitOversizedFile(f, max)...)
			continue
		}
		if cur.Len()+len(f) > max {
			flush()
		}
		cur.WriteString(f)
	}
	flush()
	return chunks
}

// splitOversizedFile breaks one file's diff that exceeds max into pieces no
// larger than max, cutting on line boundaries. Pieces after the first are
// prefixed with the file's "diff --git" header so each piece still names its
// file (the first piece already begins with that header).
func splitOversizedFile(chunk string, max int) []string {
	header := ""
	if p := diffFilePath(chunk); p != "" {
		header = "diff --git a/" + p + " b/" + p + "\n"
	}
	room := max - len(header)
	if room < 512 {
		room = 512 // guarantee forward progress even with a tiny budget
	}

	var pieces []string
	rest := chunk
	for len(rest) > 0 {
		end := len(rest)
		if end > room {
			end = lineBoundary(rest, room)
		}
		piece := rest[:end]
		if len(pieces) > 0 && header != "" {
			piece = header + piece
		}
		pieces = append(pieces, piece)
		rest = rest[end:]
	}
	return pieces
}

// lineBoundary returns an index <= max just after the last newline in s[:max], or
// max if there is none, so a slice never splits mid-line unless a single line is
// itself longer than max.
func lineBoundary(s string, max int) int {
	if i := strings.LastIndexByte(s[:max], '\n'); i >= 0 {
		return i + 1
	}
	return max
}

// diffSummary lists the files a diff touches with their added/removed line
// counts, largest change first. It gives the model an at-a-glance map of the
// whole commit so a single-line suggestion is not anchored on whichever file
// sorts first: `git diff` orders files by path, so an uppercase name like
// README.md leads even when the real work is elsewhere. Returns "" for a
// single-file diff, where no such map is needed.
func diffSummary(diff string) string {
	type stat struct {
		path           string
		added, removed int
	}
	var stats []stat
	for _, f := range splitDiffByFile(diff) {
		path := diffFilePath(f)
		if path == "" {
			continue
		}
		s := stat{path: path}
		for _, line := range strings.Split(f, "\n") {
			switch {
			case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
				// file headers, not content
			case strings.HasPrefix(line, "+"):
				s.added++
			case strings.HasPrefix(line, "-"):
				s.removed++
			}
		}
		stats = append(stats, s)
	}
	if len(stats) <= 1 {
		return ""
	}
	sort.SliceStable(stats, func(i, j int) bool {
		return stats[i].added+stats[i].removed > stats[j].added+stats[j].removed
	})
	var b strings.Builder
	fmt.Fprintf(&b, "Files changed (%d), largest first:\n", len(stats))
	for _, s := range stats {
		fmt.Fprintf(&b, "  %s (+%d -%d)\n", s.path, s.added, s.removed)
	}
	return b.String()
}

// diffFilePath extracts the file path from a per-file diff chunk's leading
// "diff --git a/PATH b/PATH" line. Returns "" if the chunk has no such header.
func diffFilePath(chunk string) string {
	line := chunk
	if i := strings.IndexByte(chunk, '\n'); i >= 0 {
		line = chunk[:i]
	}
	const prefix = "diff --git a/"
	if !strings.HasPrefix(line, prefix) {
		return ""
	}
	rest := line[len(prefix):]
	if i := strings.Index(rest, " b/"); i >= 0 {
		return rest[:i]
	}
	return rest
}

// splitDiffByFile breaks a unified diff into one chunk per file, each beginning
// with its "diff --git " header line. Non-diff input is returned as one chunk.
func splitDiffByFile(diff string) []string {
	const marker = "diff --git "
	if !strings.HasPrefix(diff, marker) {
		return []string{diff}
	}
	parts := strings.Split(diff, "\n"+marker)
	for i := 1; i < len(parts); i++ {
		parts[i] = marker + parts[i]
	}
	return parts
}

// indent prefixes every non-empty line of s with prefix, for readable display
// of a multi-line commit message.
func indent(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, line := range lines {
		if line != "" {
			lines[i] = prefix + line
		}
	}
	return strings.Join(lines, "\n")
}

// stripComments removes git comment lines (those starting with '#') and trims
// surrounding whitespace, leaving just the human-written message.
func stripComments(s string) string {
	var keep []string
	for _, line := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		keep = append(keep, line)
	}
	return strings.TrimSpace(strings.Join(keep, "\n"))
}
