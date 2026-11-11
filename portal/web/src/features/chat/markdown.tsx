// Tiny Markdown renderer for chat replies. It builds React elements
// only (never innerHTML), so model output cannot inject markup. It
// covers what chat answers use: fenced code, headings, lists, quotes,
// paragraphs, **bold**, *italic*, `code` and [links](https://…).

import { Fragment, type ReactNode } from "react";
import { CodeBlock } from "../../ui/code-block";

type Block =
  | { kind: "code"; lang: string; text: string }
  | { kind: "heading"; level: number; text: string }
  | { kind: "list"; ordered: boolean; items: string[] }
  | { kind: "quote"; text: string }
  | { kind: "para"; text: string };

function parseBlocks(src: string): Block[] {
  const lines = src.replace(/\r\n/g, "\n").split("\n");
  const out: Block[] = [];
  let i = 0;
  while (i < lines.length) {
    const line = lines[i];
    const fence = line.match(/^\s*```\s*([\w+-]*)/);
    if (fence) {
      const body: string[] = [];
      i++;
      while (i < lines.length && !/^\s*```/.test(lines[i])) body.push(lines[i++]);
      i++; // closing fence (or EOF while streaming)
      out.push({ kind: "code", lang: fence[1] ?? "", text: body.join("\n") });
      continue;
    }
    if (line.trim() === "") {
      i++;
      continue;
    }
    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      out.push({ kind: "heading", level: h[1].length, text: h[2] });
      i++;
      continue;
    }
    const li = /^\s*(?:[-*+]|\d+[.)])\s+/;
    if (li.test(line)) {
      const ordered = /^\s*\d/.test(line);
      const items: string[] = [];
      while (i < lines.length && li.test(lines[i])) {
        let item = lines[i].replace(li, "");
        i++;
        while (i < lines.length && /^\s{2,}\S/.test(lines[i]) && !li.test(lines[i])) {
          item += " " + lines[i].trim();
          i++;
        }
        items.push(item);
      }
      out.push({ kind: "list", ordered, items });
      continue;
    }
    if (/^\s*>/.test(line)) {
      const q: string[] = [];
      while (i < lines.length && /^\s*>/.test(lines[i])) q.push(lines[i++].replace(/^\s*>\s?/, ""));
      out.push({ kind: "quote", text: q.join("\n") });
      continue;
    }
    const p: string[] = [];
    while (
      i < lines.length &&
      lines[i].trim() !== "" &&
      !/^\s*```/.test(lines[i]) &&
      !/^#{1,6}\s/.test(lines[i]) &&
      !li.test(lines[i]) &&
      !/^\s*>/.test(lines[i])
    ) {
      p.push(lines[i++]);
    }
    out.push({ kind: "para", text: p.join("\n") });
  }
  return out;
}

const inlineRe = /(`[^`\n]+`)|(\*\*[^*\n]+\*\*)|(\*[^*\n]+\*)|(\[[^\]\n]+\]\(https?:\/\/[^)\s]+\))/g;

function renderInline(text: string): ReactNode[] {
  const out: ReactNode[] = [];
  let last = 0;
  let k = 0;
  for (const m of text.matchAll(inlineRe)) {
    const start = m.index ?? 0;
    if (start > last) out.push(...withBreaks(text.slice(last, start), k++));
    const tok = m[0];
    if (m[1]) {
      out.push(
        <code key={k++} className="rounded bg-surface-2 px-1 py-0.5 font-mono text-[0.85em]">
          {tok.slice(1, -1)}
        </code>,
      );
    } else if (m[2]) {
      out.push(<strong key={k++}>{tok.slice(2, -2)}</strong>);
    } else if (m[3]) {
      out.push(<em key={k++}>{tok.slice(1, -1)}</em>);
    } else {
      const lm = tok.match(/^\[([^\]]+)\]\((https?:\/\/[^)\s]+)\)$/)!;
      out.push(
        <a key={k++} href={lm[2]} target="_blank" rel="noopener noreferrer" className="text-brand underline">
          {lm[1]}
        </a>,
      );
    }
    last = start + tok.length;
  }
  if (last < text.length) out.push(...withBreaks(text.slice(last), k++));
  return out;
}

function withBreaks(text: string, key: number): ReactNode[] {
  const parts = text.split("\n");
  return parts.map((p, i) => (
    <Fragment key={`${key}-${i}`}>
      {p}
      {i < parts.length - 1 && <br />}
    </Fragment>
  ));
}

export function Markdown({ text }: { text: string }) {
  const blocks = parseBlocks(text);
  return (
    <div className="space-y-3 text-sm leading-relaxed text-fg">
      {blocks.map((b, i) => {
        switch (b.kind) {
          case "code":
            return <CodeBlock key={i} code={b.text} />;
          case "heading":
            return (
              <p key={i} className={b.level <= 2 ? "text-base font-semibold" : "font-semibold"}>
                {renderInline(b.text)}
              </p>
            );
          case "list": {
            const items = b.items.map((it, j) => <li key={j}>{renderInline(it)}</li>);
            return b.ordered ? (
              <ol key={i} className="list-decimal space-y-1 pl-5">
                {items}
              </ol>
            ) : (
              <ul key={i} className="list-disc space-y-1 pl-5">
                {items}
              </ul>
            );
          }
          case "quote":
            return (
              <blockquote key={i} className="border-l-2 border-border pl-3 text-muted">
                {renderInline(b.text)}
              </blockquote>
            );
          default:
            return <p key={i}>{renderInline(b.text)}</p>;
        }
      })}
    </div>
  );
}
