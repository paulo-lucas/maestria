import crypto from 'node:crypto';
import type { Audit } from './audit.ts';
import type { Db } from './db.ts';
import type { EventStore } from './events.ts';
import { ValidationError, type ActorContext } from './inbox.ts';
import { renderMarkdown } from './web/markdown.ts';

export type ChatSender = 'human' | 'maestro';

export interface ChatMessage {
  id: string;
  workspaceId: string;
  sender: ChatSender;
  author: string;
  body: string;
  bodyHtml: string;
  createdAt: number;
}

interface ChatRow {
  id: string;
  workspace_id: string;
  sender: ChatSender;
  author: string;
  body: string;
  created_at: number;
}

export const MAX_CHAT_BODY = 16_000;
export const MAX_CHAT_AUTHOR = 64;

function toMessage(row: ChatRow): ChatMessage {
  return {
    id: row.id,
    workspaceId: row.workspace_id,
    sender: row.sender,
    author: row.author,
    body: row.body,
    bodyHtml: renderMarkdown(row.body),
    createdAt: row.created_at,
  };
}

export class ChatStore {
  constructor(
    private db: Db,
    private audit?: Audit,
    private events?: EventStore,
    private now: () => number = Date.now,
  ) {}

  send(
    workspaceId: string,
    input: { sender: ChatSender; author?: string; body: string },
    actor: ActorContext = { via: 'cli' },
  ): ChatMessage {
    if (typeof input.body !== 'string') throw new ValidationError('Chat message body must be a string');
    const body = input.body.trim();
    if (!body) throw new ValidationError('Chat message cannot be empty');
    if (body.length > MAX_CHAT_BODY) {
      throw new ValidationError(`Chat message must be at most ${MAX_CHAT_BODY} characters`);
    }

    const defaultAuthor = input.sender === 'human' ? 'Human' : 'Maestro';
    const rawAuthor = input.author === undefined ? defaultAuthor : input.author.trim();
    if (!rawAuthor || rawAuthor.length > MAX_CHAT_AUTHOR) {
      throw new ValidationError(`Author must be 1..${MAX_CHAT_AUTHOR} characters`);
    }

    const id = crypto.randomUUID();
    const t = this.now();
    this.db
      .prepare('INSERT INTO chat_messages (id, workspace_id, sender, author, body, created_at) VALUES (?, ?, ?, ?, ?, ?)')
      .run(id, workspaceId, input.sender, rawAuthor, body, t);

    this.audit?.log('chat_send', {
      ip: actor.ip ?? null,
      detail: { workspaceId, messageId: id, sender: input.sender, author: rawAuthor, via: actor.via },
    });

    if (input.sender === 'human') {
      this.events?.append(workspaceId, 'chat', {
        messageId: id,
        author: rawAuthor,
        body,
        createdAt: t,
      });
    }

    return {
      id,
      workspaceId,
      sender: input.sender,
      author: rawAuthor,
      body,
      bodyHtml: renderMarkdown(body),
      createdAt: t,
    };
  }

  list(workspaceId: string, opts: { limit?: number } = {}): ChatMessage[] {
    const limit = Math.max(1, Math.min(opts.limit ?? 200, 500));
    const rows = this.db
      .prepare(
        `SELECT id, workspace_id, sender, author, body, created_at
         FROM chat_messages
         WHERE workspace_id = ?
         ORDER BY created_at DESC, id DESC
         LIMIT ?`,
      )
      .all(workspaceId, limit) as ChatRow[];
    rows.reverse();
    return rows.map(toMessage);
  }
}
