package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf16"

	"github.com/jadewisemann/yet-another-t3-code/server/internal/store"
)

// Generated thread titles (spec/behavior/titles.md). A thread launched
// with generateTitle keeps its seed title while `codex exec` writes one,
// then takes the generated title unless the user renamed it meanwhile.

const (
	titleModel           = "gpt-6-luna"
	titleReasoningEffort = "low"
	titleTimeout         = 180 * time.Second
	maxThreadTitleChars  = 120
)

var placeholderThreadTitle = regexp.MustCompile(` thread \d+$`)

// titleSetBeforeFirstMessage reports whether the thread was named on
// purpose before its first message (upstream hasTitleSetBeforeFirstMessage).
func titleSetBeforeFirstMessage(t *store.Thread, seed string) bool {
	if t.TitleRenamed != nil && *t.TitleRenamed {
		return true
	}
	title := strings.TrimSpace(t.Title)
	return title != "" && title != "New thread" && !placeholderThreadTitle.MatchString(title) &&
		title != strings.TrimSpace(seed)
}

type titleAttachment struct {
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	SizeBytes int64  `json:"sizeBytes"`
}

// generateTitle runs one title request and records its outcome.
func (s *Server) generateTitle(threadID, requestID, messageID string) {
	var (
		message, projectTitle, cwd string
		attachments                []titleAttachment
		stale                      bool
	)
	s.store.Read(func(st *store.State, _ int64) {
		t, ok := st.Threads[threadID]
		if !ok || t.TitleRegeneration == nil || t.TitleRegeneration.RequestID != requestID {
			stale = true
			return
		}
		if m := t.Proj.Get("messages", messageID); m != nil {
			var msg struct {
				Text        string            `json:"text"`
				Streaming   bool              `json:"streaming"`
				Attachments []titleAttachment `json:"attachments"`
			}
			if json.Unmarshal(m.Payload, &msg) == nil && !msg.Streaming {
				message, attachments = msg.Text, msg.Attachments
			}
		}
		if p, ok := st.Projects[t.ProjectID]; ok {
			projectTitle, cwd = p.Title, p.WorkspaceRoot
		}
		if t.WorktreePath != nil {
			if info, err := os.Stat(*t.WorktreePath); err == nil && info.IsDir() {
				cwd = *t.WorktreePath
			}
		}
	})
	if stale {
		return
	}
	var title string
	var err error
	switch {
	case cwd == "":
		err = errors.New("this thread's project no longer exists")
	case strings.TrimSpace(message) == "" && len(attachments) == 0:
		err = errors.New("this thread has no messages to title yet")
	default:
		prompt := threadTitlePrompt(message, projectTitle, attachments)
		// Upstream retries a first title twice, two and four seconds apart.
		for attempt, delay := 0, 2*time.Second; ; attempt, delay = attempt+1, delay*2 {
			title, err = s.runTitleGeneration(cwd, prompt)
			if err == nil || attempt == 2 {
				break
			}
			time.Sleep(delay)
		}
	}
	if err != nil {
		s.log.Warn("thread title generation failed", slog.String("thread", threadID), slog.String("error", err.Error()))
		title = ""
	} else if strings.TrimSpace(title) == "New thread" {
		title = ""
	}
	now := store.Now()
	_, derr := s.store.Decide(context.Background(), requestID+":title-complete", func(st *store.State) (string, any, error) {
		t, ok := st.Threads[threadID]
		if !ok || t.TitleRegeneration == nil || t.TitleRegeneration.RequestID != requestID {
			return "", nil, errStaleTitle
		}
		meta := store.ThreadMetadata{ThreadID: threadID, TitleRegenerationDone: true, At: now}
		if title != "" {
			renamed := false
			meta.Title, meta.TitleRenamed = &title, &renamed
		}
		return store.ThreadMetadataUpdated, meta, nil
	})
	if derr != nil && !errors.Is(derr, errStaleTitle) && !errors.Is(derr, store.ErrDuplicate) {
		s.log.Warn("thread title", slog.String("error", derr.Error()))
	}
}

var errStaleTitle = errors.New("title request is no longer current")

// runTitleGeneration asks `codex exec` for a title, the way upstream
// CodexTextGeneration does, and returns it sanitized.
func (s *Server) runTitleGeneration(cwd, prompt string) (string, error) {
	tmp, err := os.MkdirTemp("", "yatt-title-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmp)
	schemaPath, outputPath := filepath.Join(tmp, "schema.json"), filepath.Join(tmp, "output.json")
	if err := os.WriteFile(schemaPath, []byte(threadTitleSchema), 0o600); err != nil {
		return "", err
	}
	if err := os.WriteFile(outputPath, nil, 0o600); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(context.Background(), titleTimeout)
	defer cancel()
	bin := s.runs.codexBinary()
	off := s.enabledMcpServers(ctx, bin, cwd)
	args := []string{"exec"}
	if len(off) > 0 {
		parts := make([]string, len(off))
		for i, name := range off {
			q, _ := json.Marshal(name)
			parts[i] = string(q) + "={enabled=false}"
		}
		args = append(args, "--config", "mcp_servers={"+strings.Join(parts, ",")+"}")
	}
	args = append(args, codexNoPluginArgs...)
	for _, name := range off {
		if !strings.ContainsAny(name, ".=") {
			args = append(args, "--config", "mcp_servers."+name+".enabled=false")
		}
	}
	args = append(args,
		"--ephemeral", "--skip-git-repo-check", "-s", "read-only",
		"--model", s.runs.titleModel(ctx, cwd, titleModel),
		"--config", fmt.Sprintf("model_reasoning_effort=%q", titleReasoningEffort),
		"--config", "features.codex_hooks=false", "--config", "features.hooks=false",
		"--output-schema", schemaPath, "--output-last-message", outputPath, "-")
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = cwd
	cmd.Stdin = strings.NewReader(prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", errors.New("Codex CLI request timed out")
		}
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			detail = strings.TrimSpace(stdout.String())
		}
		if detail == "" {
			return "", fmt.Errorf("Codex CLI command failed: %v", err)
		}
		return "", fmt.Errorf("Codex CLI command failed: %s", detail)
	}
	data, err := os.ReadFile(outputPath)
	if err != nil {
		return "", err
	}
	var out struct {
		Title *string `json:"title"`
	}
	if err := json.Unmarshal(extractJSONObject(data), &out); err != nil || out.Title == nil {
		return "", errors.New("Codex returned invalid structured output")
	}
	title := sanitizeThreadTitle(*out.Title)
	// Some models echo the output schema's key instead of a title.
	if strings.ToLower(title) == "title" {
		return "", errors.New(`the model returned the placeholder title "title"`)
	}
	return title, nil
}

func extractJSONObject(data []byte) []byte {
	start, end := bytes.IndexByte(data, '{'), bytes.LastIndexByte(data, '}')
	if start < 0 || end < start {
		return data
	}
	return data[start : end+1]
}

// Plugins and apps add MCP servers that could act on the prompt.
var codexNoPluginArgs = []string{
	"--config", "features.connectors=false",
	"--config", "features.plugins=false",
	"--config", "features.apps=false",
}

// enabledMcpServers lists the user's enabled MCP servers so title
// generation can turn them off; any failure keeps them as they are.
func (s *Server) enabledMcpServers(ctx context.Context, bin, cwd string) []string {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, append(append([]string{}, codexNoPluginArgs...), "mcp", "list", "--json")...)
	cmd.Dir = cwd
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var servers []struct {
		Name    string `json:"name"`
		Enabled bool   `json:"enabled"`
	}
	if json.Unmarshal(out, &servers) != nil {
		return nil
	}
	var names []string
	for _, srv := range servers {
		if srv.Enabled {
			names = append(names, srv.Name)
		}
	}
	return names
}

// titleModel picks the text generation model the account can run: the
// requested one, one of its family, or the account's default.
func (rn *runner) titleModel(ctx context.Context, cwd, requested string) string {
	c, err := rn.ensureClient(ctx, cwd)
	if err != nil {
		return requested
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := c.Call(ctx, "model/list", map[string]any{})
	if err != nil {
		return requested
	}
	var list struct {
		Data []struct {
			Model     string `json:"model"`
			IsDefault bool   `json:"isDefault"`
			Hidden    bool   `json:"hidden"`
		} `json:"data"`
	}
	if json.Unmarshal(res, &list) != nil || len(list.Data) == 0 {
		return requested
	}
	family := func(slug string) string { return strings.TrimPrefix(slug, "openai.") }
	for _, m := range list.Data {
		if m.Model == requested {
			return m.Model
		}
	}
	for _, m := range list.Data {
		if family(m.Model) == family(requested) {
			return m.Model
		}
	}
	for _, m := range list.Data {
		if m.IsDefault {
			return m.Model
		}
	}
	return list.Data[0].Model
}

const threadTitleSchema = `{"type":"object","properties":{"title":{"type":"string"},"needsRefinement":{"type":"boolean"}},"required":["title","needsRefinement"],"additionalProperties":false}`

var quotedTitle = regexp.MustCompile("^(['\"`])(.*)$")

// sanitizeThreadTitle is upstream sanitizeThreadTitle: one line, outer
// quotes and extra spaces removed, at most 120 UTF-16 units.
func sanitizeThreadTitle(raw string) string {
	title := raw
	var decoded struct {
		Title *string `json:"title"`
	}
	if json.Unmarshal([]byte(raw), &decoded) == nil && decoded.Title != nil {
		title = *decoded.Title
	}
	title = strings.TrimSpace(title)
	title = strings.TrimSpace(strings.Split(strings.ReplaceAll(title, "\r\n", "\n"), "\n")[0])
	if m := quotedTitle.FindStringSubmatch(title); m != nil && len(m[2]) > 0 &&
		strings.HasSuffix(m[2], m[1]) && !strings.Contains(m[2][:len(m[2])-1], m[1]) {
		title = m[2][:len(m[2])-1]
	}
	title = strings.Join(strings.Fields(title), " ")
	if title == "" {
		return "New thread"
	}
	units := utf16.Encode([]rune(title))
	if len(units) <= maxThreadTitleChars {
		return title
	}
	end := maxThreadTitleChars - 3
	if end > 0 && end < len(units) && utf16.IsSurrogate(rune(units[end-1])) && units[end-1] < 0xdc00 {
		end--
	}
	return strings.TrimRight(string(utf16.Decode(units[:end])), " \t\n\r") + "..."
}

// limitTitleMessage keeps the start and end of a long message (upstream
// limitTitleMessage); lengths are UTF-16 units like JavaScript strings.
func limitTitleMessage(text string, budget int) string {
	const truncated = "\n[Content truncated]\n"
	units := utf16.Encode([]rune(text))
	if len(units) <= budget {
		return text
	}
	available := budget - len(truncated)
	head := (available + 1) / 2
	tail := available - head
	if head > 0 && units[head-1] >= 0xd800 && units[head-1] <= 0xdbff {
		head--
	}
	if tail > 0 {
		if first := units[len(units)-tail]; first >= 0xdc00 && first <= 0xdfff {
			tail--
		}
	}
	out := string(utf16.Decode(units[:head])) + truncated
	if tail > 0 {
		out += string(utf16.Decode(units[len(units)-tail:]))
	}
	return out
}

// threadTitlePrompt is upstream buildThreadTitlePrompt for a first title
// without user title instructions, linked context or a fork source.
func threadTitlePrompt(message, projectTitle string, attachments []titleAttachment) string {
	avoid := "- Avoid project names already visible in the UI, quotes, labels, filler, and trailing punctuation."
	if name := strings.TrimSpace(projectTitle); name != "" {
		q, _ := json.Marshal(name)
		avoid = "- Never include the project name (" + string(q) + "); the UI already shows it next to the title. Avoid quotes, labels, filler, and trailing punctuation."
	}
	prompt := strings.ReplaceAll(initialTitlePrompt, "{{avoid}}", avoid) + "\n\nUser message:\n" + limitTitleMessage(message, 8000)
	if len(attachments) > 0 {
		lines := make([]string, len(attachments))
		for i, a := range attachments {
			lines[i] = fmt.Sprintf("- %s (%s, %d bytes)", a.Name, a.MimeType, a.SizeBytes)
		}
		section := strings.Join(lines, "\n")
		if u := utf16.Encode([]rune(section)); len(u) > 4000 {
			section = string(utf16.Decode(u[:4000])) + "\n\n[truncated]"
		}
		prompt += "\n\nAttachment metadata:\n" + section
	}
	return prompt
}

const initialTitlePrompt = `Generate a title that will help the user recognize this T3 Code thread weeks later.
Return JSON with keys title and needsRefinement.
Set needsRefinement to true only if the subject is still unknown, such as an unresolved link, "fix this", or an unexplained attachment. Otherwise set it to false.

Before answering, silently reduce the request to:
- Subject: What system, feature, or problem is this really about?
- Outcome: What does the user ultimately want to understand or change?
- Incidental instructions: What only describes how the agent should do the work?

Title the subject and outcome. Discard incidental instructions.

Editorial rules:
- 3-8 words, fewer than 40 characters.
- Use a compact noun phrase or clear action phrase.
- Capture the umbrella goal when the request lists several symptoms or steps.
- When asked about thread behavior, never include the referenced titles' topics. "[Fix login] why didn't this thread start?" means "Thread Startup Failure". Use linked topics only for requests about that work.
- Name the product change, not the mock, plan, report, branch, or PR used to produce it.
- Models, subagents, tools, output formats, and monitoring instructions do not belong in the title unless they are themselves the topic.
- For reviews, name what is being reviewed and the relevant concern. Avoid generic titles such as "Review PR 123" when linked or attached context reveals the subject.
- For research, name the question domain rather than the requested research process.
- Do not claim the work is complete.
- Do not copy and truncate the user's message.
{{avoid}}
- Use attached images as primary context for UI issues.
- When a URL or attachment is the only source of the subject, use available tools to inspect it directly.
- Local git history is not evidence of what a linked PR or issue is about. Never title the thread after branch names, commit messages, or merged commits found in the checkout.
- If a linked PR or issue cannot be read, fall back to the user's stated action plus its number, such as "Take Over PR 8588". This is the one case where a PR or issue number belongs in the title.`
