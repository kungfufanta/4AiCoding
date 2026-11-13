// Chat MVP (dashboard BFF): conversations with the platform LLM,
// streamed to the browser as Server-Sent Events. No tools and no
// sandbox access yet; A2/A3 (docs/AGENT_PLAN.md) attach model
// connections and agent runs to these same conversations.
//
// Conversations are private to their author: every query runs under
// WithOrg with app.user_id pinned, and the RLS policy on both tables
// matches org AND user (migration 0005_chat.sql). A foreign or
// unknown conversation id answers 404.
package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/tastyeffectco/sandboxd/portal/internal/db"
	"github.com/tastyeffectco/sandboxd/portal/internal/llm"
	"github.com/tastyeffectco/sandboxd/portal/internal/llmconfig"
)

const (
	chatMaxInputRunes   = 16000 // one user message
	chatHistoryMessages = 40    // turns sent back to the model
	chatHistoryBytes    = 96 << 10
	chatTitleRunes      = 60
	chatDefaultTitle    = "New chat"
)

// chatSystemPrompt frames the assistant. Kept short: the hub may add
// its own memory context on top.
const chatSystemPrompt = "You are the 4AiCoding assistant inside the Portal dashboard. " +
	"Help users plan and write code, websites and sandbox workflows. " +
	"Answer in the user's language and format answers in Markdown. " +
	"You cannot run code or open sandboxes yet; say so if asked."

type chatConversationJSON struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	// A3.4: the sandbox pinned to this chat (decision §13-3) — the
	// Chat page needs it to restore the attach state after a reload
	// when the chat has no runs yet.
	SandboxID string    `json:"sandbox_id,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type chatMessageJSON struct {
	ID      string `json:"id"`
	Role    string `json:"role"`
	Content string `json:"content"`
	// A3.4: assistant notes written by the worker (run summary,
	// failed-run note). The Chat page hides them for the run it
	// renders as a run card — the card shows the same content.
	AgentRunID string    `json:"agent_run_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// handleChatStatus tells the SPA whether chat is wired, which model
// answers by default and which models the picker may offer (A2).
// Resolution order: org default → platform default → env fallback.
//
// The route is org-scoped (GET /api/orgs/{orgID}/chat/status) so the
// resolver can see DB-configured org + platform connections; the
// previous org-less /api/chat/status path left orgID empty and the
// resolver answered "empty org id" — which made the chat textarea
// appear disabled even after the admin added a platform connection.
// Membership is checked first so a foreign org id answers 404 (no
// existence leak).
func (s *Server) handleChatStatus(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	model := ""
	models := []string{}
	if k, err := llmconfig.ResolveEffective(r.Context(), s.Pool, orgID); err == nil {
		model = k.DefaultModel
		for _, m := range k.Models {
			models = append(models, m.ID)
		}
	} else if s.LLM != nil {
		model = s.LLM.Model
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"enabled": model != "",
		"model":   model,
		"models":  models,
	})
}

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	out := []chatConversationJSON{}
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		rows, err := tx.Query(r.Context(), `
                        SELECT id::text, title, COALESCE(sandbox_id,''), created_at, updated_at
                        FROM chat_conversations
                        ORDER BY updated_at DESC
                        LIMIT 100`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var c chatConversationJSON
			if err := rows.Scan(&c.ID, &c.Title, &c.SandboxID, &c.CreatedAt, &c.UpdatedAt); err != nil {
				return err
			}
			out = append(out, c)
		}
		return rows.Err()
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not list chats")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	var c chatConversationJSON
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		return tx.QueryRow(r.Context(), `
                        INSERT INTO chat_conversations (org_id, user_id, title)
                        VALUES ($1::uuid, $2::uuid, $3)
                        RETURNING id::text, title, created_at, updated_at`,
			orgID, db.UserID(r.Context()), chatDefaultTitle).
			Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt)
	})
	if err != nil {
		s.Log.Error("create chat failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not create chat")
		return
	}
	writeJSON(w, http.StatusCreated, c)
}

// chatID returns the {chatID} path value when it is a UUID.
func chatID(r *http.Request) (string, bool) {
	id := r.PathValue("chatID")
	if _, err := uuid.Parse(id); err != nil {
		return "", false
	}
	return id, true
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	id, ok := chatID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	var n int64
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		tag, err := tx.Exec(r.Context(), `DELETE FROM chat_conversations WHERE id = $1::uuid`, id)
		n = tag.RowsAffected()
		return err
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not delete chat")
		return
	}
	if n == 0 {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) handleListChatMessages(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	id, ok := chatID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	out := []chatMessageJSON{}
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		if err := chatExists(r.Context(), tx, id); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `
                        SELECT id::text, role, content, COALESCE(agent_run_id::text,''), created_at
                        FROM chat_messages
                        WHERE conversation_id = $1::uuid
                        ORDER BY created_at, id`, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var m chatMessageJSON
			if err := rows.Scan(&m.ID, &m.Role, &m.Content, &m.AgentRunID, &m.CreatedAt); err != nil {
				return err
			}
			out = append(out, m)
		}
		return rows.Err()
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "internal", "could not load messages")
		return
	}
	writeJSON(w, http.StatusOK, out)
}

// chatExists answers pgx.ErrNoRows when RLS hides the conversation
// (foreign user/org) or it does not exist.
func chatExists(ctx context.Context, tx db.Tx, id string) error {
	var one int
	return tx.QueryRow(ctx, `SELECT 1 FROM chat_conversations WHERE id = $1::uuid`, id).Scan(&one)
}

// chatTitle derives a conversation title from the first user message.
func chatTitle(content string) string {
	t := strings.Join(strings.Fields(content), " ")
	if utf8.RuneCountInString(t) <= chatTitleRunes {
		return t
	}
	r := []rune(t)
	return strings.TrimSpace(string(r[:chatTitleRunes])) + "…"
}

// trimHistory keeps the newest turns within the message and byte
// budgets, oldest first.
func trimHistory(msgs []llm.Message) []llm.Message {
	if len(msgs) > chatHistoryMessages {
		msgs = msgs[len(msgs)-chatHistoryMessages:]
	}
	total := 0
	start := len(msgs)
	for start > 0 {
		n := len(msgs[start-1].Content)
		if total+n > chatHistoryBytes && start < len(msgs) {
			break
		}
		total += n
		start--
	}
	return msgs[start:]
}

// handleSendChatMessage stores the user's message, then streams the
// model's reply as SSE events:
//
//	event: user   data: {"message": {...}}             the stored user turn
//	event: delta  data: {"text": "..."}                each fragment
//	event: done   data: {"message": {...}, "title": "…"} the stored reply
//	event: error  data: {"code": "...", "message": "..."}
//
// Errors before streaming starts use the normal JSON envelope.
func (s *Server) handleSendChatMessage(w http.ResponseWriter, r *http.Request) {
	orgID := pathOrgID(r)
	if _, ok := s.requireMember(w, r, orgID); !ok {
		return
	}
	id, ok := chatID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	if s.LLM == nil {
		// A2: even without the env fallback, a DB-configured
		// connection may exist. Try resolving before 503.
		if _, err := s.resolveChatClient(r.Context(), orgID); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "chat_not_configured",
				"chat is not configured (no model connection for this org and no PORTAL_DEFAULT_LLM_API_KEY)")
			return
		}
	}
	r.Body = http.MaxBytesReader(w, r.Body, 128<<10)
	var body struct {
		Content string `json:"content"`
		// Model (A2): optional per-message model override. Must be in
		// the effective connection's allowed list; empty uses the
		// connection's default_model.
		Model string `json:"model,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "bad_request", `body must be {"content": "..."}`)
		return
	}
	content := strings.TrimSpace(body.Content)
	if content == "" || utf8.RuneCountInString(content) > chatMaxInputRunes {
		writeErr(w, http.StatusBadRequest, "bad_request", "content must be 1..16000 characters")
		return
	}

	userID := db.UserID(r.Context())
	var history []llm.Message
	var userMsg chatMessageJSON
	title := ""
	err := db.WithOrg(r.Context(), s.Pool, orgID, func(tx db.Tx) error {
		var cur string
		if err := tx.QueryRow(r.Context(),
			`SELECT title FROM chat_conversations WHERE id = $1::uuid FOR UPDATE`, id).Scan(&cur); err != nil {
			return err
		}
		rows, err := tx.Query(r.Context(), `
                        SELECT role, content FROM (
                            SELECT role, content, created_at, id FROM chat_messages
                            WHERE conversation_id = $1::uuid
                            ORDER BY created_at DESC, id DESC
                            LIMIT $2) t
                        ORDER BY created_at, id`, id, chatHistoryMessages)
		if err != nil {
			return err
		}
		for rows.Next() {
			var m llm.Message
			if err := rows.Scan(&m.Role, &m.Content); err != nil {
				rows.Close()
				return err
			}
			history = append(history, m)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if err := tx.QueryRow(r.Context(), `
                        INSERT INTO chat_messages (conversation_id, org_id, user_id, role, content)
                        VALUES ($1::uuid, $2::uuid, $3::uuid, 'user', $4)
                        RETURNING id::text, role, content, created_at`,
			id, orgID, userID, content).Scan(&userMsg.ID, &userMsg.Role, &userMsg.Content, &userMsg.CreatedAt); err != nil {
			return err
		}
		if cur == chatDefaultTitle && len(history) == 0 {
			title = chatTitle(content)
		}
		_, err = tx.Exec(r.Context(), `
                        UPDATE chat_conversations
                        SET updated_at = now(), title = COALESCE(NULLIF($2, ''), title)
                        WHERE id = $1::uuid`, id, title)
		return err
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "not_found", "chat not found")
		return
	}
	if err != nil {
		s.Log.Error("store chat message failed", slog.Any("error", err))
		writeErr(w, http.StatusInternalServerError, "internal", "could not store message")
		return
	}

	msgs := append([]llm.Message{{Role: "system", Content: chatSystemPrompt}},
		trimHistory(append(history, llm.Message{Role: "user", Content: content}))...)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	send := func(event string, data any) error {
		raw, _ := json.Marshal(data)
		if _, err := w.Write([]byte("event: " + event + "\ndata: " + string(raw) + "\n\n")); err != nil {
			return err
		}
		return rc.Flush()
	}
	_ = send("user", map[string]any{"message": userMsg})

	// A2: resolve the effective LLM connection for this org. Falls
	// back to s.LLM (env-based) when no DB connection is configured.
	chatClient, err := s.resolveChatClient(r.Context(), orgID)
	if err != nil && s.LLM != nil {
		// Copy: the model override below must not change the shared
		// env client other requests use.
		c := *s.LLM
		chatClient = &c
	}
	if chatClient == nil {
		writeErr(w, http.StatusServiceUnavailable, "chat_not_configured",
			"chat is not configured (no model connection for this org)")
		return
	}
	// Model override (A2): when the caller picked a model, verify it
	// is in the connection's allowed list before using it. An empty
	// override keeps the connection's default_model.
	if body.Model != "" {
		if !s.modelAllowed(r.Context(), orgID, body.Model) {
			writeErr(w, http.StatusBadRequest, "model_not_allowed",
				"model "+body.Model+" is not in the connection's allowed list")
			return
		}
		chatClient.Model = body.Model
	}
	reply, streamErr := chatClient.Stream(r.Context(), llm.Request{
		Messages:  msgs,
		UserID:    userID,
		SessionID: id,
		RequestID: userMsg.ID,
	}, func(d string) error { return send("delta", map[string]string{"text": d}) })

	// Keep whatever arrived, even if the browser left mid-stream.
	saveCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 10*time.Second)
	defer cancel()
	var asst chatMessageJSON
	if strings.TrimSpace(reply) != "" {
		if err := db.WithOrg(saveCtx, s.Pool, orgID, func(tx db.Tx) error {
			if err := tx.QueryRow(saveCtx, `
                                INSERT INTO chat_messages (conversation_id, org_id, user_id, role, content)
                                VALUES ($1::uuid, $2::uuid, $3::uuid, 'assistant', $4)
                                RETURNING id::text, role, content, created_at`,
				id, orgID, userID, reply).Scan(&asst.ID, &asst.Role, &asst.Content, &asst.CreatedAt); err != nil {
				return err
			}
			_, err := tx.Exec(saveCtx, `UPDATE chat_conversations SET updated_at = now() WHERE id = $1::uuid`, id)
			return err
		}); err != nil {
			s.Log.Error("store chat reply failed", slog.Any("error", err))
		}
	}

	if streamErr != nil {
		if r.Context().Err() != nil {
			return // client gone; nothing to tell
		}
		s.Log.Warn("chat stream failed", slog.Any("error", streamErr))
		_ = send("error", map[string]string{
			"code":    "upstream",
			"message": "the model did not answer; try again",
		})
		return
	}
	if asst.ID == "" {
		_ = send("error", map[string]string{"code": "empty_reply", "message": "the model returned an empty reply"})
		return
	}
	_ = send("done", map[string]any{"message": asst, "title": title})
}

// resolveChatClient returns the llm.Client to use for one chat call.
// Resolution order (AGENT_PLAN A2 §5.2):
//  1. the org's is_default connection (from model_connections);
//  2. the platform's is_default connection;
//  3. env PORTAL_DEFAULT_LLM_* (s.LLM, the Chat MVP fallback).
//
// The DB lookup is best-effort: if the DB is unreachable or no
// connection is configured, the function returns ErrNotFound so the
// caller can fall back to s.LLM. The decrypted key is never logged.
func (s *Server) resolveChatClient(ctx context.Context, orgID string) (*llm.Client, error) {
	k, err := llmconfig.ResolveEffective(ctx, s.Pool, orgID)
	if err != nil {
		return nil, err
	}
	return &llm.Client{
		BaseURL: k.BaseURL,
		APIKey:  k.APIKey, // plaintext; never logged
		Model:   k.DefaultModel,
		Flavor:  k.Flavor,
		HTTP:    s.llmHTTPFor(k.OrgID, 0),
	}, nil
}

// modelAllowed reports whether model is in the effective connection's
// allowed list (or the list is empty = every model allowed). Used to
// gate the per-message model override from the Chat model picker.
func (s *Server) modelAllowed(ctx context.Context, orgID, model string) bool {
	k, err := llmconfig.ResolveEffective(ctx, s.Pool, orgID)
	if err != nil {
		return false // no connection = no override
	}
	if len(k.Models) == 0 {
		return true // open list
	}
	for _, m := range k.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}
