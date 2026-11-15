// Landing page (issue Z1 #79): public intro at "/" for guests. A
// signed-in visitor never sees it — the router's HomeGate sends them
// straight to Chat (issue Z1 #81). Vietnamese, styled with the design
// tokens the shell already ships (bg/surface/brand), no new UI deps.

import { Link } from "react-router";
import { ArrowRight, Bot, Code2, MessageSquare, ShieldCheck } from "lucide-react";
import { cn } from "../../lib/cn";

const primaryLink =
  "inline-flex h-11 items-center justify-center gap-2 rounded-[var(--radius)] bg-brand px-5 text-sm font-medium text-brand-fg shadow-sm transition-colors hover:bg-brand-hover";
const secondaryLink =
  "inline-flex h-11 items-center justify-center gap-2 rounded-[var(--radius)] border border-border bg-surface px-5 text-sm font-medium text-fg transition-colors hover:bg-surface-2";

const FEATURES = [
  {
    icon: MessageSquare,
    title: "Chat AI",
    text: "Hỏi đáp, viết code, giải thích tài liệu — câu trả lời stream từng dòng ngay trong trình duyệt.",
  },
  {
    icon: Bot,
    title: "Agent làm việc trong sandbox",
    text: "Agent tự viết và chạy code trong sandbox riêng của bạn, mỗi run có checkpoint để hoàn tác an toàn.",
  },
  {
    icon: Code2,
    title: "API cho developer",
    text: "REST API, SDK và MCP server gắn agent vào workflow của bạn. Xem trang Quickstart sau khi đăng nhập.",
    link: { to: "/quickstart", label: "Xem Quickstart" },
  },
];

export function LandingPage() {
  return (
    <div className="flex min-h-dvh flex-col bg-bg">
      {/* Top bar */}
      <header className="mx-auto flex w-full max-w-5xl items-center gap-2.5 px-4 py-4 sm:px-6">
        <span className="flex size-8 items-center justify-center rounded-xl bg-brand text-brand-fg">
          <Bot aria-hidden className="size-4.5" />
        </span>
        <span className="text-base font-semibold tracking-tight text-fg">4AiCoding</span>
        <Link to="/login" state={{ from: "/" }} className={cn(secondaryLink, "ml-auto h-9 px-3.5")}>
          Đăng nhập
        </Link>
      </header>

      {/* Hero */}
      <main className="mx-auto w-full max-w-5xl flex-1 px-4 sm:px-6">
        <section className="pt-12 pb-16 text-center sm:pt-20">
          <h1 className="mx-auto max-w-2xl text-3xl leading-tight font-semibold tracking-tight text-fg sm:text-5xl">
            Chat với AI agent. Agent làm việc cho bạn.
          </h1>
          <p className="mx-auto mt-4 max-w-xl text-base text-muted sm:text-lg">
            Chat với AI agent, agent viết và chạy code trong sandbox riêng của bạn.
          </p>
          <div className="mt-8 flex flex-col items-center justify-center gap-3 sm:flex-row">
            <Link to="/login" state={{ from: "/" }} className={primaryLink}>
              Bắt đầu chat miễn phí
              <ArrowRight aria-hidden className="size-4" />
            </Link>
            <Link to="/login" state={{ from: "/" }} className={secondaryLink}>
              Đăng nhập
            </Link>
          </div>
        </section>

        {/* Feature blocks */}
        <section className="grid gap-4 pb-16 sm:grid-cols-3">
          {FEATURES.map(({ icon: Icon, title, text, link }) => (
            <div
              key={title}
              className="flex flex-col rounded-xl border border-border bg-surface p-5 shadow-sm"
            >
              <span className="flex size-9 items-center justify-center rounded-lg bg-brand-soft text-brand">
                <Icon aria-hidden className="size-4.5" />
              </span>
              <h2 className="mt-3 text-base font-semibold text-fg">{title}</h2>
              <p className="mt-1.5 text-sm leading-relaxed text-muted">{text}</p>
              {link && (
                <Link
                  to={link.to}
                  className="mt-3 inline-flex items-center gap-1 text-sm font-medium text-brand hover:underline"
                >
                  {link.label}
                  <ArrowRight aria-hidden className="size-3.5" />
                </Link>
              )}
            </div>
          ))}
        </section>

        {/* Trust line */}
        <section className="pb-16 text-center">
          <p className="inline-flex items-center gap-2 text-sm text-muted">
            <ShieldCheck aria-hidden className="size-4" />
            Code của bạn chạy trong sandbox cách ly — không trộn lẫn, có checkpoint, có nhật ký.
          </p>
        </section>
      </main>

      <footer className="border-t border-border py-5 text-center text-xs text-muted">
        © Devhub Solutions
      </footer>
    </div>
  );
}
