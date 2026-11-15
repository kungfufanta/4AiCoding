// Chat side panel for an attached sandbox: a "Code" tab (read-only file
// tree + viewer over the sandboxd /files API) and a "Preview" tab (the
// sandbox's web preview in an iframe), like the original sandboxd
// console. The file tree refreshes while an agent run is active.

import { useMemo, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import {
  ChevronDown,
  ChevronRight,
  Code2,
  ExternalLink,
  File,
  Folder,
  Globe,
  Loader2,
  RefreshCw,
  X,
} from "lucide-react";
import { api, ApiError, type Sandbox, type SandboxFileEntry } from "../../api";
import { cn } from "../../lib/cn";
import { Button } from "../../ui/button";

type Tab = "code" | "preview";

// Workspace root as the files API sees it (paths are relative to
// /home/sandbox); falls back to the whole home when it does not exist.
const ROOT = "workspace";

type TreeNode = { name: string; path: string; type: "file" | "dir"; size?: number; children: TreeNode[] };

function buildTree(entries: SandboxFileEntry[], root: string): TreeNode[] {
  const top: TreeNode[] = [];
  const dirs = new Map<string, TreeNode>();
  const prefix = root ? root + "/" : "";
  for (const e of entries) {
    const rel = prefix && e.path.startsWith(prefix) ? e.path.slice(prefix.length) : e.path;
    const parts = rel.split("/");
    const node: TreeNode = { name: parts[parts.length - 1], path: e.path, type: e.type, size: e.size, children: [] };
    if (e.type === "dir") dirs.set(e.path, node);
    const parentPath = e.path.includes("/") ? e.path.slice(0, e.path.lastIndexOf("/")) : "";
    const parent = dirs.get(parentPath);
    (parent ? parent.children : top).push(node);
  }
  const sort = (nodes: TreeNode[]) => {
    nodes.sort((a, b) => (a.type === b.type ? a.name.localeCompare(b.name) : a.type === "dir" ? -1 : 1));
    nodes.forEach((n) => sort(n.children));
  };
  sort(top);
  return top;
}

export function WorkspacePanel({
  orgID,
  sandbox,
  sandboxID,
  live,
  onClose,
  className,
}: {
  orgID: string;
  sandbox?: Sandbox;
  sandboxID: string;
  live: boolean; // an agent run is active: poll the tree
  onClose: () => void;
  className?: string;
}) {
  const [tab, setTab] = useState<Tab>("code");
  return (
    <section
      className={cn("flex min-h-0 min-w-0 flex-col rounded-xl border border-border bg-surface", className)}
      aria-label="Không gian làm việc"
    >
      <header className="flex items-center gap-1 border-b border-border px-2 py-1.5">
        <div className="flex rounded-[var(--radius)] border border-border p-0.5" role="tablist" aria-label="Chế độ xem">
          {(
            [
              { id: "code", label: "Code", icon: Code2 },
              { id: "preview", label: "Preview", icon: Globe },
            ] as const
          ).map((t) => (
            <button
              key={t.id}
              role="tab"
              aria-selected={tab === t.id}
              onClick={() => setTab(t.id)}
              className={cn(
                "flex items-center gap-1.5 rounded-[calc(var(--radius)-2px)] px-2.5 py-1 text-xs transition-colors",
                tab === t.id ? "bg-brand-soft font-medium text-brand" : "text-muted hover:text-fg",
              )}
            >
              <t.icon aria-hidden className="size-3.5" />
              {t.label}
            </button>
          ))}
        </div>
        <span className="ml-2 truncate font-mono text-[11px] text-muted">{sandboxID}</span>
        <Button variant="ghost" size="sm" className="ml-auto" aria-label="Đóng bảng Code/Preview" onClick={onClose}>
          <X aria-hidden className="size-4" />
        </Button>
      </header>
      {tab === "code" ? (
        <CodeTab orgID={orgID} sandboxID={sandboxID} live={live} />
      ) : (
        <PreviewTab sandbox={sandbox} />
      )}
    </section>
  );
}

function CodeTab({ orgID, sandboxID, live }: { orgID: string; sandboxID: string; live: boolean }) {
  const [selected, setSelected] = useState<string>("");
  const [open, setOpen] = useState<Record<string, boolean>>({});

  const files = useQuery({
    queryKey: ["sandbox-files", orgID, sandboxID],
    queryFn: async () => {
      try {
        return { root: ROOT, list: await api.listSandboxFiles(orgID, sandboxID, ROOT) };
      } catch (e) {
        if (e instanceof ApiError && e.status === 404) {
          return { root: "", list: await api.listSandboxFiles(orgID, sandboxID, "") };
        }
        throw e;
      }
    },
    refetchInterval: live ? 4000 : false,
  });
  const tree = useMemo(
    () => (files.data ? buildTree(files.data.list.entries, files.data.root) : []),
    [files.data],
  );

  const content = useQuery({
    queryKey: ["sandbox-file", orgID, sandboxID, selected],
    queryFn: () => api.getSandboxFile(orgID, sandboxID, selected),
    enabled: selected !== "",
    refetchInterval: live ? 4000 : false,
  });

  const renderNodes = (nodes: TreeNode[], depth: number) =>
    nodes.map((n) => {
      const isOpen = open[n.path] ?? depth <= 1;
      return (
        <div key={n.path}>
          <button
            title={n.path}
            onClick={() =>
              n.type === "dir" ? setOpen((o) => ({ ...o, [n.path]: !isOpen })) : setSelected(n.path)
            }
            className={cn(
              "flex w-full items-center gap-1 truncate rounded px-1.5 py-0.5 text-left text-xs hover:bg-surface-2",
              selected === n.path ? "bg-brand-soft text-brand" : "text-fg",
            )}
            style={{ paddingLeft: 6 + depth * 12 }}
          >
            {n.type === "dir" ? (
              <>
                {isOpen ? (
                  <ChevronDown aria-hidden className="size-3 shrink-0 text-muted" />
                ) : (
                  <ChevronRight aria-hidden className="size-3 shrink-0 text-muted" />
                )}
                <Folder aria-hidden className="size-3.5 shrink-0 text-muted" />
              </>
            ) : (
              <File aria-hidden className="ml-4 size-3.5 shrink-0 text-muted" />
            )}
            <span className="truncate">{n.name}</span>
          </button>
          {n.type === "dir" && isOpen && renderNodes(n.children, depth + 1)}
        </div>
      );
    });

  return (
    <div className="grid min-h-0 flex-1 grid-cols-[minmax(120px,38%)_minmax(0,1fr)]">
      <div className="min-h-0 overflow-y-auto border-r border-border p-1.5" aria-label="Cây thư mục">
        <div className="mb-1 flex items-center gap-1 px-1">
          <span className="text-[11px] font-medium uppercase tracking-wide text-muted">Files</span>
          <Button
            variant="ghost"
            size="sm"
            className="ml-auto h-6 px-1.5"
            aria-label="Tải lại cây thư mục"
            onClick={() => void files.refetch()}
          >
            <RefreshCw aria-hidden className={cn("size-3.5", files.isFetching && "animate-spin")} />
          </Button>
        </div>
        {files.isPending ? (
          <Loader2 aria-label="Đang tải" className="m-2 size-4 animate-spin text-muted" />
        ) : files.isError ? (
          <p className="px-1.5 text-xs text-danger">
            {files.error instanceof ApiError && files.error.status === 403
              ? "Key sandboxd của Portal chưa có quyền sandboxes:files nên không xem được file."
              : `Không đọc được file: ${files.error instanceof Error ? files.error.message : String(files.error)}`}
          </p>
        ) : tree.length === 0 ? (
          <p className="px-1.5 text-xs text-muted">Workspace còn trống. Giao việc cho agent để tạo file.</p>
        ) : (
          <>
            {renderNodes(tree, 0)}
            {files.data?.list.truncated && (
              <p className="px-1.5 pt-1 text-[11px] text-muted">Danh sách bị cắt ở 2000 mục.</p>
            )}
          </>
        )}
      </div>
      <div className="flex min-h-0 min-w-0 flex-col">
        {selected === "" ? (
          <p className="m-auto px-4 text-center text-xs text-muted">Chọn một file để xem nội dung.</p>
        ) : (
          <>
            <div className="truncate border-b border-border px-3 py-1.5 font-mono text-[11px] text-muted" title={selected}>
              {selected}
            </div>
            <div className="min-h-0 flex-1 overflow-auto">
              {content.isPending ? (
                <Loader2 aria-label="Đang tải" className="m-3 size-4 animate-spin text-muted" />
              ) : content.isError ? (
                <p className="p-3 text-xs text-danger">
                  {content.error instanceof Error ? content.error.message : String(content.error)}
                </p>
              ) : content.data.encoding === "base64" ? (
                <p className="p-3 text-xs text-muted">File nhị phân ({content.data.size} byte), không hiển thị.</p>
              ) : (
                <pre className="p-3 font-mono text-[12px] leading-5 text-fg" aria-label="Nội dung file">
                  <code>{content.data.data}</code>
                  {content.data.truncated && "\n… (đã cắt, file lớn hơn 512 KiB)"}
                </pre>
              )}
            </div>
          </>
        )}
      </div>
    </div>
  );
}

function PreviewTab({ sandbox }: { sandbox?: Sandbox }) {
  const [nonce, setNonce] = useState(0);
  const preview = sandbox?.preview;
  if (!sandbox) {
    return <p className="m-auto px-4 text-center text-xs text-muted">Đang tải thông tin sandbox…</p>;
  }
  if (!preview?.url) {
    return (
      <p className="m-auto max-w-sm px-4 text-center text-xs text-muted">
        Sandbox này chưa mở cổng web nên không có preview. Sandbox tạo bằng nút “Bật Agent” mới sẽ có preview ở
        cổng 3000.
      </p>
    );
  }
  return (
    <div className="flex min-h-0 flex-1 flex-col">
      <div className="flex items-center gap-1.5 border-b border-border px-2 py-1.5">
        <Button variant="ghost" size="sm" className="h-6 px-1.5" aria-label="Tải lại preview" onClick={() => setNonce((n) => n + 1)}>
          <RefreshCw aria-hidden className="size-3.5" />
        </Button>
        <span className="min-w-0 flex-1 truncate rounded bg-surface-2 px-2 py-0.5 font-mono text-[11px] text-muted" title={preview.url}>
          {preview.url}
        </span>
        <a
          href={preview.url}
          target="_blank"
          rel="noreferrer"
          aria-label="Mở preview ở tab mới"
          className="rounded p-1 text-muted hover:bg-surface-2 hover:text-fg"
        >
          <ExternalLink aria-hidden className="size-3.5" />
        </a>
      </div>
      {preview.status !== "running" ? (
        <p className="m-auto px-4 text-center text-xs text-muted">Sandbox đang dừng; bật lại để xem preview.</p>
      ) : (
        <iframe
          key={nonce}
          src={preview.url}
          title="Preview web của sandbox"
          className="min-h-0 w-full flex-1 rounded-b-xl bg-white"
          sandbox="allow-scripts allow-forms allow-same-origin allow-popups allow-modals"
        />
      )}
    </div>
  );
}
