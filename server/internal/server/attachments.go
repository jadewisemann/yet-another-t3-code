package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

// Attachments (spec/behavior/attachments.md): a client asks for a signed
// upload URL, posts the bytes there, and names the upload in a message.
// Uploads wait in <userdata>/attachments as pending-<uuid>[-ext]; the
// message that uses one renames it after its thread. The agent gets the
// saved paths in the message text, and images as image input.

const (
	attachmentUploadPrefix = "/api/attachments/upload/"
	attachmentUploadTTL    = 10 * time.Minute
	maxImageAttachment     = 10 << 20
	maxFileAttachment      = 25 << 20
)

var (
	attachmentKeyOnce sync.Once
	attachmentKey     []byte

	imageExtensionByMime = map[string]string{
		"image/avif": ".avif", "image/bmp": ".bmp", "image/gif": ".gif", "image/heic": ".heic",
		"image/heif": ".heif", "image/jpeg": ".jpg", "image/jpg": ".jpg", "image/png": ".png",
		"image/svg+xml": ".svg", "image/tiff": ".tiff", "image/webp": ".webp",
	}
	safeImageExtensions = map[string]bool{
		".avif": true, ".bmp": true, ".gif": true, ".heic": true, ".heif": true, ".ico": true,
		".jpeg": true, ".jpg": true, ".png": true, ".svg": true, ".tiff": true, ".webp": true,
	}
	// Images the agents take as image input.
	agentImageMimes = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}

	attachmentIDRe = regexp.MustCompile(`^([a-z0-9_-]+?)-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})(?:-([a-z0-9]{1,10}))?$`)
	fileExtRe      = regexp.MustCompile(`^\.[a-z0-9]{1,10}$`)
	nameExtRe      = regexp.MustCompile(`(?i)\.([a-z0-9]{1,8})$`)
	unsafeSegment  = regexp.MustCompile(`[^a-z0-9_-]+`)
	dashes         = regexp.MustCompile(`-+`)
)

func uploadKey() []byte {
	attachmentKeyOnce.Do(func() {
		attachmentKey = make([]byte, 32)
		_, _ = rand.Read(attachmentKey)
	})
	return attachmentKey
}

type uploadClaims struct {
	Version      int    `json:"version"`
	Kind         string `json:"kind"`
	Type         string `json:"type"`
	AttachmentID string `json:"attachmentId"`
	Name         string `json:"name"`
	MimeType     string `json:"mimeType"`
	SizeBytes    int64  `json:"sizeBytes"`
	ExpiresAt    int64  `json:"expiresAt"`
}

func (s *Server) attachmentsDir() string {
	return filepath.Join(UserData(s.cfg.BaseDir), "attachments")
}

// fileExtension is the stored extension of a file attachment.
func fileExtension(name string) string {
	ext := strings.ToLower(filepath.Ext(name))
	if ext == ".part" || !fileExtRe.MatchString(ext) {
		return ".bin"
	}
	return ext
}

// imageExtension is the stored extension of an image attachment.
func imageExtension(mime, name string) string {
	if ext, ok := imageExtensionByMime[strings.ToLower(mime)]; ok {
		return ext
	}
	if m := nameExtRe.FindStringSubmatch(strings.TrimSpace(name)); m != nil && safeImageExtensions["."+strings.ToLower(m[1])] {
		return "." + strings.ToLower(m[1])
	}
	return ".bin"
}

func attachmentExtension(kind, mime, name string) string {
	if kind == "file" {
		return fileExtension(name)
	}
	return imageExtension(mime, name)
}

func (s *Server) createUploadURL(ctx context.Context, payload json.RawMessage) (any, error) {
	var in struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		MimeType  string `json:"mimeType"`
		SizeBytes int64  `json:"sizeBytes"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	if in.Type == "" {
		in.Type = "image"
	}
	limit := int64(maxImageAttachment)
	if in.Type == "file" {
		limit = maxFileAttachment
	}
	if in.SizeBytes < 1 || in.SizeBytes > limit {
		return nil, fmt.Errorf("attachment size must be between 1 and %d bytes", limit)
	}
	name := in.Name
	if in.Type == "file" && strings.EqualFold(in.MimeType, "image/svg+xml") && !strings.HasSuffix(strings.ToLower(name), ".svg") {
		name += ".svg"
	}
	id, err := newUUID()
	if err != nil {
		return nil, err
	}
	id = "pending-" + id
	if in.Type == "file" {
		id += "-" + strings.TrimPrefix(fileExtension(name), ".")
	}
	expires := time.Now().Add(attachmentUploadTTL).UnixMilli()
	claims, _ := json.Marshal(uploadClaims{Version: 1, Kind: "attachment-upload", Type: in.Type, AttachmentID: id,
		Name: name, MimeType: in.MimeType, SizeBytes: in.SizeBytes, ExpiresAt: expires})
	encoded := base64.RawURLEncoding.EncodeToString(claims)
	return map[string]any{
		"attachmentId": id,
		"relativeUrl":  attachmentUploadPrefix + encoded + "." + signUpload(encoded),
		"expiresAt":    expires,
	}, nil
}

func signUpload(payload string) string {
	m := hmac.New(sha256.New, uploadKey())
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func validUpload(token string) *uploadClaims {
	parts := strings.Split(token, ".")
	if len(parts) != 2 || !hmac.Equal([]byte(signUpload(parts[0])), []byte(parts[1])) {
		return nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil
	}
	var c uploadClaims
	if json.Unmarshal(raw, &c) != nil || c.Kind != "attachment-upload" || c.ExpiresAt <= time.Now().UnixMilli() {
		return nil
	}
	if !attachmentIDRe.MatchString(c.AttachmentID) {
		return nil
	}
	return &c
}

// uploadAttachment stores the body of a signed upload URL. It needs no
// bearer token: the signature is the permission.
func (s *Server) uploadAttachment(w http.ResponseWriter, r *http.Request) {
	c := validUpload(strings.TrimPrefix(r.URL.Path, attachmentUploadPrefix))
	if c == nil {
		http.Error(w, "invalid or expired upload URL", http.StatusForbidden)
		return
	}
	dir := s.attachmentsDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	final := filepath.Join(dir, c.AttachmentID+attachmentExtension(c.Type, c.MimeType, c.Name))
	part, err := os.CreateTemp(dir, c.AttachmentID+".*.part")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	n, err := io.Copy(part, io.LimitReader(r.Body, c.SizeBytes+1))
	closeErr := part.Close()
	if err == nil {
		err = closeErr
	}
	if err == nil && n != c.SizeBytes {
		err = fmt.Errorf("Body was %d bytes, expected %d.", n, c.SizeBytes)
		_ = os.Remove(part.Name())
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err == nil {
		err = os.Rename(part.Name(), final)
	}
	if err != nil {
		_ = os.Remove(part.Name())
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteAttachment(ctx context.Context, payload json.RawMessage) (any, error) {
	var in struct {
		AttachmentID string `json:"attachmentId"`
	}
	if err := json.Unmarshal(payload, &in); err != nil {
		return nil, err
	}
	// Only uploads no message has claimed yet.
	if strings.HasPrefix(in.AttachmentID, "pending-") && attachmentIDRe.MatchString(in.AttachmentID) {
		if path := s.attachmentPath(in.AttachmentID); path != "" {
			_ = os.Remove(path)
		}
	}
	return nil, nil
}

// attachmentPath is where an attachment's bytes are saved, or "".
func (s *Server) attachmentPath(id string) string {
	if !attachmentIDRe.MatchString(id) {
		return ""
	}
	matches, _ := filepath.Glob(filepath.Join(s.attachmentsDir(), id+".*"))
	for _, m := range matches {
		if !strings.HasSuffix(m, ".part") && !strings.Contains(strings.TrimPrefix(filepath.Base(m), id+"."), ".") {
			return m
		}
	}
	return ""
}

// threadAttachmentSegment is the thread part of a claimed attachment id.
func threadAttachmentSegment(threadID string) string {
	seg := strings.ToLower(strings.TrimSpace(threadID))
	seg = dashes.ReplaceAllString(unsafeSegment.ReplaceAllString(seg, "-"), "-")
	seg = strings.Trim(seg, "-_")
	if len(seg) > 80 {
		seg = seg[:80]
	}
	seg = strings.TrimRight(seg, "-_")
	if seg == "pending" {
		return "_pending"
	}
	return seg
}

type messageAttachment struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	MimeType  string          `json:"mimeType"`
	SizeBytes int64           `json:"sizeBytes"`
	Source    json.RawMessage `json:"source,omitempty"`
}

// claimAttachments moves a message's pending uploads to the thread and
// returns the attachments with their new ids.
func (s *Server) claimAttachments(threadID string, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return json.RawMessage("[]"), nil
	}
	var list []messageAttachment
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, err
	}
	segment := threadAttachmentSegment(threadID)
	for i, a := range list {
		m := attachmentIDRe.FindStringSubmatch(a.ID)
		if m == nil || m[1] != "pending" || segment == "" {
			continue
		}
		from := s.attachmentPath(a.ID)
		if from == "" {
			return nil, fmt.Errorf("attachment %s was not uploaded", a.ID)
		}
		id := segment + "-" + m[2]
		if m[3] != "" {
			id += "-" + m[3]
		}
		to := filepath.Join(s.attachmentsDir(), id+strings.TrimPrefix(filepath.Base(from), a.ID))
		if err := os.Rename(from, to); err != nil {
			return nil, err
		}
		list[i].ID = id
	}
	return json.Marshal(list)
}

// parseAttachments reads a message's attachments, ignoring what it cannot.
func parseAttachments(raw json.RawMessage) []messageAttachment {
	var list []messageAttachment
	_ = json.Unmarshal(raw, &list)
	return list
}

// textWithAttachmentPaths appends where each attachment is saved to the
// message text, as upstream providerMessageTextWithAttachmentPaths does.
func (s *Server) textWithAttachmentPaths(text string, list []messageAttachment) string {
	for _, a := range list {
		var context string
		if path := s.attachmentPath(a.ID); path == "" {
			context = fmt.Sprintf("[Attached %s \"%s\" is unavailable: could not resolve a saved path for attachment '%s'.]", a.Type, a.Name, a.ID)
		} else {
			context = fmt.Sprintf("[Attached %s \"%s\" is saved at: %s]", a.Type, a.Name, path)
		}
		if text == "" {
			text = context
		} else {
			text += "\n\n" + context
		}
	}
	return text
}

// agentImages are the attachments an agent takes as images, with their
// bytes base64-encoded.
func (s *Server) agentImages(list []messageAttachment) (images []struct{ mime, data string }, err error) {
	for _, a := range list {
		if a.Type != "image" || !agentImageMimes[strings.ToLower(a.MimeType)] {
			continue
		}
		path := s.attachmentPath(a.ID)
		if path == "" {
			return nil, errors.New("attachment not found: " + a.ID)
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		images = append(images, struct{ mime, data string }{strings.ToLower(a.MimeType), base64.StdEncoding.EncodeToString(b)})
	}
	return images, nil
}
