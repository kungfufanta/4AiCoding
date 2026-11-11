-- Chat MVP: conversations with the platform LLM (WebHub glm-4.6 by
-- default, AGENT_PLAN 5.2.1). A3 later attaches agent runs and tools
-- to the same conversations.
--
-- Conversations are private to their author inside the org: RLS pins
-- both the org (app.org_id) and the user (app.user_id), so neither an
-- org-mate nor a forgotten WHERE clause can read someone else's chat.

CREATE TABLE IF NOT EXISTS chat_conversations (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    org_id     UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    title      TEXT NOT NULL DEFAULT 'New chat',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS chat_conversations_recent
    ON chat_conversations (org_id, user_id, updated_at DESC);

CREATE TABLE IF NOT EXISTS chat_messages (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    conversation_id UUID NOT NULL REFERENCES chat_conversations(id) ON DELETE CASCADE,
    org_id          UUID NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
    user_id         UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role            TEXT NOT NULL CHECK (role IN ('user', 'assistant')),
    content         TEXT NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS chat_messages_conv
    ON chat_messages (conversation_id, created_at);

ALTER TABLE chat_conversations ENABLE ROW LEVEL SECURITY;
CREATE POLICY chat_conversations_owner ON chat_conversations FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
       AND user_id = NULLIF(current_setting('app.user_id', true), '')::uuid);

ALTER TABLE chat_messages ENABLE ROW LEVEL SECURITY;
CREATE POLICY chat_messages_owner ON chat_messages FOR ALL
    USING (org_id = NULLIF(current_setting('app.org_id', true), '')::uuid
       AND user_id = NULLIF(current_setting('app.user_id', true), '')::uuid);

GRANT SELECT, INSERT, UPDATE, DELETE ON chat_conversations TO portal_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON chat_messages TO portal_app;
