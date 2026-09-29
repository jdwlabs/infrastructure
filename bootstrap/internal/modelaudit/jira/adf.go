// Package jira is the slice of Jira Cloud REST v3 the audit uses: JQL search,
// comments, create and comment, with bodies in Atlassian Document Format.
package jira

import (
	"encoding/json"
	"strings"
)

// Node is one ADF node. Version is set only on the root doc.
type Node struct {
	Type    string         `json:"type"`
	Version int            `json:"version,omitempty"`
	Attrs   map[string]any `json:"attrs,omitempty"`
	Content []Node         `json:"content,omitempty"`
	Text    string         `json:"text,omitempty"`
	Marks   []Mark         `json:"marks,omitempty"`
}

type Mark struct {
	Type  string         `json:"type"`
	Attrs map[string]any `json:"attrs,omitempty"`
}

func Doc(content ...Node) Node { return Node{Type: "doc", Version: 1, Content: content} }

func Paragraph(inline ...Node) Node { return Node{Type: "paragraph", Content: inline} }

func Text(s string) Node { return Node{Type: "text", Text: s} }

func Link(s, href string) Node {
	return Node{Type: "text", Text: s, Marks: []Mark{{Type: "link", Attrs: map[string]any{"href": href}}}}
}

func Heading(level int, s string) Node {
	return Node{Type: "heading", Attrs: map[string]any{"level": level}, Content: []Node{Text(s)}}
}

// BulletList makes one list item per entry, each a paragraph of inline nodes.
func BulletList(items ...[]Node) Node {
	list := Node{Type: "bulletList"}
	for _, inline := range items {
		list.Content = append(list.Content, Node{Type: "listItem", Content: []Node{Paragraph(inline...)}})
	}
	return list
}

func CodeBlock(language, text string) Node {
	return Node{Type: "codeBlock", Attrs: map[string]any{"language": language}, Content: []Node{Text(text)}}
}

// CandidatesLanguage marks the machine-readable candidate list.
const CandidatesLanguage = "audit-candidates"

// CandidatesHeader is the block's first line. Dedupe also matches on it, in
// case the Jira editor rewrites a language it does not know.
const CandidatesHeader = "# audit-candidates"

// CandidatesBlock renders repo ids as the block dedupe reads back.
func CandidatesBlock(repos []string) Node {
	return CodeBlock(CandidatesLanguage, strings.Join(append([]string{CandidatesHeader}, repos...), "\n"))
}

// CandidateRepos walks an ADF document and returns every repo id listed in
// an audit-candidates code block. A body that is not ADF yields nothing.
func CandidateRepos(raw json.RawMessage) []string {
	var root Node
	if len(raw) == 0 || json.Unmarshal(raw, &root) != nil {
		return nil
	}
	var repos []string
	var walk func(n Node)
	walk = func(n Node) {
		if n.Type == "codeBlock" {
			text := PlainText(n)
			lines := strings.Split(text, "\n")
			lang, _ := n.Attrs["language"].(string)
			if lang == CandidatesLanguage || strings.TrimSpace(lines[0]) == CandidatesHeader {
				for _, l := range lines {
					l = strings.TrimSpace(l)
					if l != "" && !strings.HasPrefix(l, "#") {
						repos = append(repos, l)
					}
				}
			}
			return
		}
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(root)
	return repos
}

// PlainText concatenates every text node under n.
func PlainText(n Node) string {
	var b strings.Builder
	var walk func(Node)
	walk = func(n Node) {
		b.WriteString(n.Text)
		for _, c := range n.Content {
			walk(c)
		}
	}
	walk(n)
	return b.String()
}

// PlainTextOf is PlainText for a raw ADF body.
func PlainTextOf(raw json.RawMessage) string {
	var n Node
	if json.Unmarshal(raw, &n) != nil {
		return ""
	}
	return PlainText(n)
}
