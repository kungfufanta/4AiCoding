# KẾ HOẠCH — 4AiCoding

4AiCoding là một **Admin Orchestrator quản lý vòng đời sandbox qua API thuần**. Dự án được fork từ [`tastyeffectco/sandboxd`](https://github.com/tastyeffectco/sandboxd) (MIT, commit `92f4ffd`), sau đó bỏ toàn bộ tầng AI agent, giữ lại và mở rộng tầng điều khiển.

Sau này agent (nếu có) chỉ là **một client khác** của API, không có đặc quyền gì hơn GUI hay `curl`, và luôn bị coi là **không đáng tin** (xem mục 8).

## 0. Nguyên tắc bất biến

1. **Declarative**: image, command, port, domain, egress, resource và lifecycle đều khai báo qua spec. Không có hành vi "tự động ngầm".
2. **Governed by policy**: admin đặt policy ở tầng instance, và mọi request tạo sandbox bị clamp trong biên đó.
3. **Auditable**: mọi thao tác được ghi vào `audit_log` và `events`.
4. **Composable**: GUI, CLI, CI và agent tương lai dùng chung một API surface.
5. **Zero trust với workload**: mọi thứ chạy trong sandbox là không đáng tin. Kiểm soát được thực thi bằng code ở control plane, nằm ngoài tầm với của sandbox.

## 1. Quyết định nền tảng

| Vấn đề | Quyết định |
|---|---|
| Vị trí code | Ghi đè trực tiếp vào `kungfufanta/4AiCoding`, ở thư mục gốc. Không fork riêng. `docs/` và `.vscode/` cũ được giữ. |
| Nhánh làm việc | `claude/project-thread-y21txm`, mở PR vào `main` sau mỗi checkpoint |
| Ngôn ngữ / stack | Go 1.26 (`control-plane/`). Console là React 19 + TypeScript (giữ stack hiện có, viết lại ở M4) |
| Auth | Username/password bcrypt trong SQLite, session cookie `HttpOnly` và CSRF (đã có sẵn một phần trong `internal/console` và `internal/auth`) |
| Admin đầu tiên | Env `SANDBOXD_BOOTSTRAP_ADMIN=user:pass` (lần khởi động đầu) **và** subcommand `sandboxd users add` |
| TLS mặc định | `off` (HTTP). Bật `letsencrypt` hoặc `custom` qua `/v1/domain` khi chạy production |
| `gitimport` + `detect` | **Giữ**: GUI cấu hình sandbox có ô nhập repo URL và tự nhận diện ngôn ngữ. `detect` cần `manifest`, `recipes` và `preset`, nên 3 package này cũng được giữ (chỉ là dữ liệu nhận diện framework) |
| `snapshot` | **Giữ**: phục vụ standby/restore |
| Distributed | Hỗ trợ cả single-node và multi-worker (gateway + worker fleet) |

## 2. Kiến trúc

**Chế độ A, all-in-one** (dev hoặc một VPS nhỏ): `sandboxd` (control plane + data plane), Traefik và SQLite chạy trên một Docker daemon.

**Chế độ B, distributed** (production): control plane giữ API, GUI, policy, domain và SQLite (state + audit). Worker đăng ký qua `/internal` bằng worker token.

- **Worker trên VPS (Docker)**: `sandboxd --node-id <id> --capacity-cpus N --capacity-memory-mb M --capacity-disk-mb D --control-plane-url <url> --worker-token <token>`. Worker tự heartbeat và báo capacity. `node_id` được giữ ổn định qua restart nhờ state dir.
- **Worker trên k3s**: B1 chạy worker như một pod có mount socket runtime của node, giúp lên k3s nhanh. B2 viết runtime adapter Kubernetes (Pod/Service/Ingress/NetworkPolicy). Lộ trình là B1 ở M7, rồi B2 khi cần.

> ⚠️ Mount `docker.sock` chỉ được làm cho **tiến trình worker**, không bao giờ cho sandbox. Xem mục 8.

## 3. Phạm vi code

### ✅ Giữ (Go, trong `control-plane/`)
`cmd/sandboxd`, `internal/{docker, store, reaper, wake, loopback, reconcile, traefik, previewhost, sandboxspec, egress, auth, console, metrics, activity, events, audit, logging, instancecfg, idlock, cgroup, snapshot, secrets, gitimport, detect, manifest, recipes, preset}`.

`internal/console` chứa các primitive auth (bcrypt, session token), **không phải** giao diện web.

### ❌ Đã bỏ ở M1
- **Go**: `cmd/runtimed`, `internal/{runtime, authproxy, agentauth, agentcatalog, agentprompt, modelprovider, modelconfig, upgrade, telemetry, nginx}`. `internal/runtime` chỉ là client nói chuyện với `runtimed` qua Unix socket, không phải interface Docker, nên được xoá hẳn.
- **Route trong `internal/api`**: các nhóm `tasks`, `agents`, `agent-instances`, `agent-catalog`, `apps`, `model-providers`, `files`, `git`, `git-credentials`, `terminal`, `claude-login`, `presets`, `runtime/*`, `upgrade`, `export`, `processes/*/logs`.
- **Non-Go**: `desktop/`, `examples/`, `image/templates/`, workflow `desktop*.yml`, job `agent-providers` trong CI. Agent CLI và `runtimed` cũng được gỡ khỏi `image/Dockerfile`.

### ❌ Đã bỏ ở M2
- Bảng `app`, `task`, `agent_instance`, `app_config`, `git_credential`, `model_provider` và các cột `app_id`/`task_id` (migration `0027_drop_agent_tables.sql`); code `store` tương ứng.
- Field `template`, `runtime_preset` và `app_id` của create request. Git chỉ clone repo public (`credential_id` bị từ chối 400).
- Scope API key `upgrade`, thay bằng scope `health` (`/healthz`, `/readyz`, `/version`, `/v1/auth/status`).
- `console/` (UI agent cũ), `install.sh`, `install.ps1`, `uninstall.sh`, `upgrade.sh`, `release.sh`, `deploy/`, `image/upgrader`, `release-images.yml` và các tài liệu agent trong `docs/`.

### ⏳ Còn sót (dọn dần ở các milestone sau)
- Một số tài liệu trong `docs/` và `image/README` (isolation, gvisor, base image, HOME_LAYOUT) vẫn nhắc `runtimed`/agent ở vài chỗ.

### 🆕 Xây mới
Image theo từng sandbox, chuẩn hoá route `/v1/*`, session login, port policy, domain manager, policy engine, lifecycle API, metrics theo sandbox, các lớp bảo mật ở mục 8, GUI, CI/e2e, và worker mode.

## 4. API surface (hợp đồng đích)

```
# Auth
POST   /v1/auth/login | /v1/auth/logout     GET /v1/auth/me

# Sandbox lifecycle
GET    /v1/sandboxes                         POST /v1/sandboxes (sandboxspec)
GET    /v1/sandboxes/{id}                    DELETE /v1/sandboxes/{id}
POST   /v1/sandboxes/{id}/start | stop | restart
POST   /v1/sandboxes/{id}/exec               { cmd: string[], tty?: bool }
GET    /v1/sandboxes/{id}/logs?tail=N&follow=true   (SSE khi follow)

# Lifecycle / keep-alive
GET|PATCH /v1/sandboxes/{id}/lifecycle       { idle_timeout_secs, keep_alive }
POST|DELETE /v1/sandboxes/{id}/keep-alive

# Observability
GET    /v1/sandboxes/{id}/metrics | /metrics/stream (SSE) | /volumes

# Network
GET|PUT /v1/sandboxes/{id}/network/egress    { allow: [...] }
GET|PUT|POST /v1/sandboxes/{id}/network/ports
DELETE /v1/sandboxes/{id}/network/ports/{container_port}

# Domain
GET|PUT /v1/domain    POST /v1/domain/verify    GET|POST /v1/domain/certificates

# Policy (chỉ admin)
GET|PUT /v1/policy | /v1/policy/{ports,images,resources,egress}

# Approvals (human-in-the-loop, mục 8)
GET  /v1/approvals            POST /v1/approvals/{id}/approve | /deny

# Node / cluster
GET /v1/nodes   GET /v1/nodes/{id}/capacity   GET /v1/capacity
POST /v1/nodes/{id}/drain | /undrain
GET /v1/cluster/members   GET /v1/cluster/sandbox-index

# Meta
GET /v1/images   GET /v1/config   GET /v1/healthz   GET /v1/version   GET /metrics

# Legacy (giữ tạm, deprecate sau): POST /sandbox, GET /sandboxes, ...
```

### `sandboxspec` (body của `POST /v1/sandboxes`)

```jsonc
{
  "image": "alpine:3.19",
  "command": ["sleep", "infinity"],
  "env": { "NODE_ENV": "dev" },
  "resources": { "cpus": 1.0, "memory_mb": 512, "disk_mb": 2048 },
  "network": {
    "egress": ["github.com", "registry.npmjs.org"],
    "ports": [
      { "container": 3000 },
      { "container": 8080 }
    ]
  },
  "domain": { "subdomain": "my-app" },
  "lifecycle": { "idle_timeout_secs": 3600, "keep_alive": false },
  "workspace": { "repo_url": "...", "branch": "main" }
}
```

> Ghi chú M3a: mỗi port trong `network.ports` là một port preview (có route Traefik). Không có `expose_preview: false`, vì sandbox không publish port ra host; một port không có preview thì chỉ truy cập được từ bên trong sandbox, nên không cần khai báo. Port được lưu ở bảng `sandbox_port` có sẵn; spec (image, command, env, resources) ở bảng `sandbox_spec` (migration `0028`).

### Quy tắc áp dụng policy (phía server)
1. `image` phải nằm trong allowlist. Nếu allowlist trống, chỉ image mặc định của instance được phép.
2. Mỗi `ports[].container` phải nằm trong ít nhất một `policy_port_ranges`, nếu không trả về 400.
3. Tổng số port không vượt quá `policy.resources.max_ports_per_sandbox`.
4. `resources.*` bị **clamp** (không báo lỗi) về ngưỡng tối đa, và response trả về giá trị thực tế đã áp dụng.
5. Với `egress_mode: strict`, `egress` phải là tập con của `policy.egress`. Với `permissive` thì tự do. **Mặc định là `strict` với allowlist rỗng (deny-by-default).**
6. `lifecycle.keep_alive = true` chỉ được phép khi admin đã bật `policy.allow_keep_alive`.
7. Spec không được yêu cầu `privileged`, thêm capability, mount host path, `--network host`, hay mount `docker.sock`. Server từ chối các yêu cầu này, không có cờ nào để vượt qua.

## 5. Data model (bổ sung vào SQLite)

```
instance_config      (key, value)                    # domain, tls_mode, policy_json
sandbox_ports        (sandbox_id, container_port, host_port, protocol, expose_preview, created_at)
domain_records       (sandbox_id, subdomain, fqdn, tls_status, cert_expires_at)
policy_port_ranges   (id, min_port, max_port, protocol)
policy_images        (image, label, allowed)
nodes                (node_id, capacity_json, heartbeat_at, drain_state, address)
sandbox_placements   (sandbox_id, node_id, state, reserved_at)
approvals            (id, actor, action, target, payload_hash, status, decided_by, decided_at, expires_at)
audit_log            (id, actor, action, target, payload, ts, prev_hash, hash)   # đã có, thêm hash chain
```

## 6. GUI (SPA: Login + 5 trang)

React 19 + TypeScript, không SSR, không có WebSocket cho agent. Mọi form ánh xạ 1-1 với `sandboxspec`, và mọi nút bấm đều có lệnh `curl` tương đương trong tài liệu.

- **LoginPage**
- **SandboxListPage**: danh sách, tạo mới, start/stop/delete
- **SandboxDetailPage**: log (SSE), exec, metrics, preview URL, bật/tắt keep-alive
- **SandboxConfigPage**: image, cmd, env, resources, ports, repo URL
- **NetworkPage**: egress allowlist, các port được expose, bật/tắt preview
- **AdminPage**: domain, policy, nodes/drain, hàng đợi phê duyệt, audit log

## 7. Vòng đời và keep-alive

| Cơ chế | Hành vi |
|---|---|
| Idle reaper | Dừng sandbox sau `idle_timeout_secs` (mặc định 3600) |
| Pressure reaper | Dừng sandbox ít hoạt động khi memory PSI vượt ngưỡng |
| Wake on request | Request đầu tiên tới sandbox đã dừng sẽ chạy `docker start`, chờ port mở, và trả về trang "warming up" |
| Keep-alive | Bỏ qua idle reaper. Pressure reaper vẫn can thiệp khi host hết RAM |
| Standby | Snapshot sandbox để giải phóng RAM, request tiếp theo sẽ tự restore |

## 8. Security baseline (zero trust với workload)

Mọi thứ chạy trong sandbox, kể cả một agent "toàn quyền" sau này, đều bị coi là không đáng tin. Lớp kiểm soát nằm ở control plane và được thực thi bằng code, không dựa vào prompt.

| # | Kiểm soát | Hiện trạng (sau M2) | Milestone |
|---|---|---|---|
| 1 | **Không bao giờ mount `docker.sock`** hoặc host path tuỳ ý vào sandbox | **Đã có**: `sandboxspec.Build` chỉ mount workspace; có unit test và e2e | ✅ M3a |
| 2 | Root filesystem read-only, `--cap-drop=ALL`, `no-new-privileges`, giới hạn pids/memory, tmpfs | **Đã có** (e2e đã kiểm tra) | — |
| 3 | **Không chạy root**: ép `--user 1000:1000` bất kể USER của image | **Đã có**: mọi container chạy `1000:1000` (`SANDBOXD_SANDBOX_USER`); container cũ được tạo lại khi start | ✅ M3a |
| 4 | Runtime cách ly mạnh: gVisor (`SANDBOXD_RUNTIME=runsc`) và userns-remap | Có sẵn tuỳ chọn ở cấp instance (env) | Chưa đưa vào policy; dời sang sau M5. microVM/Kata cân nhắc sau M8 |
| 5 | **Egress deny-by-default** + allowlist domain; chặn `169.254.169.254` (metadata) và dải mạng nội bộ của host | **Đã có**: `sandboxd_net` là network `internal` (không có route ra ngoài); lối ra duy nhất là egress proxy trong `sandboxd` (`HTTP(S)_PROXY` được tiêm vào sandbox). Proxy nhận diện sandbox theo IP nguồn, chỉ cho host trong allowlist của sandbox (`PUT /v1/sandboxes/{id}/network/egress`), allowlist này phải nằm trong policy. Loopback, link-local (metadata) luôn bị chặn; IP private chỉ được khi allowlist ghi đúng IP đó. e2e kiểm tra | ✅ M3b |
| 6 | Chống DNS tunneling: sandbox chỉ dùng resolver do control plane cấp, resolver này chỉ trả lời các domain trong allowlist | **Một phần**: proxy tự resolve tên, nên sandbox không cần DNS ra ngoài để truy cập mạng. Rủi ro còn lại: DNS nhúng của Docker trên network `internal` vẫn trả lời tên container; cần xác minh nó không forward tên ngoài trên mọi phiên bản Docker | Resolver riêng: sau M5 |
| 7 | Sandbox **không kế thừa** env, mount hay network namespace của orchestrator; chỉ nhận env khai báo trong spec | **Đã có**: env được lưu trong spec; unit test và e2e kiểm tra không có biến `SANDBOXD_*` | ✅ M3a |
| 8 | **Không đưa secret vào sandbox** (env/file). Secret nằm ở control plane (`internal/secrets`, mã hoá at-rest). Sau này có Credential Broker: sandbox yêu cầu hành động, broker thực hiện thay | Secret store có sẵn; broker chưa có | Broker ở sau M5 (khi có agent client) |
| 9 | **Least privilege cho API client**: API key có scope theo tài nguyên/hành động, có thời hạn (TTL), và có thể gắn với một sandbox | **Đã có**: role (admin/operator/viewer, không vượt người tạo), scope theo nhóm route (`sandboxes:read`, `sandboxes:exec`, …), `ttl_seconds` (giới hạn bởi `SANDBOXD_API_KEY_MAX_TTL_SECONDS`) và `sandbox_id` | ✅ M3b |
| 10 | **Policy Decision Point tập trung**: mọi create/exec/network/port đi qua một hàm `policy.Decide()` duy nhất, và sandbox không có đường gọi tới API admin | **Đã có**: `internal/policy.Decide` cho create, ports, egress, lifecycle; policy lưu trong `instance_config`, sửa qua `/v1/policy`. Lưu ý: siết policy không tự thu hẹp allowlist egress đã cấp cho sandbox đang có | ✅ M3b |
| 11 | **Human-in-the-loop**: hành động rủi ro cao (xoá sandbox có `keep_alive`, đổi policy, mở egress ngoài allowlist, expose port public) bị đưa vào hàng đợi `approvals`. Mỗi phê duyệt chỉ dùng được **một lần** cho đúng payload hash, và có hạn | **Đã có**: bật từng loại qua `policy.approvals`; request trả 202 + `approval_id`, admin đăng nhập console duyệt (API key không bao giờ duyệt được), gửi lại đúng request với header `X-Sandboxd-Approval`. Duyệt hết hạn sau 24h, dùng trong 15 phút | ✅ M3c |
| 12 | **Audit tamper-evident**: `audit_log` ghi ai, làm gì, khi nào, theo policy nào, kết quả ra sao, nối với nhau bằng hash chain; có endpoint kiểm tra chuỗi | **Đã có**: mỗi dòng có `prev_hash`/`hash`; `GET /v1/audit/verify` chỉ ra dòng bị sửa hoặc xoá | ✅ M3b |
| 13 | Control plane API không bao giờ nhận request từ network mà sandbox truy cập được (`sandboxd_net`) | **Đã có**: Traefik gọi control plane qua network riêng `sandboxd_ctl` (`http://sandboxd-ctl:9000`); `sandboxd` vẫn nằm trên `sandboxd_net` để probe port khi wake, nhưng từ chối (403) mọi request có IP nguồn thuộc subnet của network đó, trừ gateway (chính là host). Lưu ý: nếu publish API ra `0.0.0.0`, sandbox có thể đi vòng qua docker-proxy của host, nên giữ `SANDBOXD_API_BIND` ở `127.0.0.1` | ✅ M3a |

## 9. Milestone

| M | Việc | Output | Trạng thái |
|---|---|---|---|
| **M0** | Chép `sandboxd` vào gốc `4AiCoding`, commit baseline | Code gốc | ✅ |
| **M1** | Xoá package/thư mục/route trong danh sách bỏ, sửa `go.mod`, base image không còn runtimed, CI mới (unit + integration Docker thật) | `go build`, `go vet`, `go test -race` xanh; e2e lifecycle xanh | ✅ |
| **M2** | Dọn phần còn lại (mục 3), chuẩn hoá `/v1/*` (thêm `GET /v1/sandboxes`, `restart`, `exec`, `logs`), giữ nhóm legacy | Route khớp mục 4 (phần lifecycle), e2e pass | ✅ |
| **M3a** | Image theo từng sandbox, `command`, `network/ports`, `metrics`, `volumes`; bảo mật mục 8 #1, #3, #7, #13 | Curl được, có unit test và e2e | ✅ |
| **M3b** | `domain`, `policy` + PDP, session login + bootstrap admin, `lifecycle/keep-alive`, egress deny-by-default, API key TTL/scope, audit hash chain | Curl được, có unit test; e2e egress | ✅ |
| **M3c** | Hàng đợi phê duyệt (human-in-the-loop) | API approvals + unit test | ✅ |
| **M4** | Viết lại `console/` (Login + 5 trang) | `pnpm build` xanh; smoke test trình duyệt với sandboxd thật | ✅ |
| **M5** | CI đầy đủ (unit + e2e + console build) + PR preview workflow | CI xanh trên PR | ✅ |
| **M6** | Worker mode: `--control-plane-url`, đăng ký fleet, `/v1/nodes/*` | 2 worker đăng ký từ 2 máy | ✅ (đăng ký + heartbeat + drain + placement; worker thực thi sandbox ở M7) |
| **M7** | k3s B1: worker pod + Helm chart tối giản | Worker k3s join fleet | |
| **M8** | k3s B2 (tuỳ chọn): runtime adapter Pod/Service/NetworkPolicy | Sandbox = Pod | |

Sau mỗi milestone có một checkpoint: báo kết quả build/test, chờ duyệt rồi mới sang milestone kế.

## 10. CI

`.github/workflows/ci.yml`:
- **unit**: `gofmt`, `go vet`, `go build`, `go test -race ./...`.
- **integration**: build `sandboxd` và image test nhỏ (`scripts/e2e/Dockerfile`), sau đó `scripts/e2e.sh` gọi API thật trên Docker của runner. Các bước: create, kiểm tra container chạy và cờ hardening, exec, validation 400/404, stop/start, delete và xác nhận container đã biến mất. Luôn dọn dẹp trong `trap`.

Từ M3a, e2e có thêm image theo request, clamp tài nguyên và từ chối port. Từ M3b có thêm egress proxy (cho phép, từ chối, chặn metadata) và policy.

Từ M5:
- **console**: `pnpm install --frozen-lockfile && pnpm build` (gồm `tsc --noEmit`).
- **integration** chạy thêm `scripts/e2e-auth.sh` với auth bật: 401 khi không có credential, login bằng bootstrap admin, CSRF, role (viewer không ghi được, user bị disable mất session ngay), API key có role/scope/TTL/gắn sandbox và thu hồi, keep-alive bị policy chặn rồi được mở, luồng approvals (202 → admin duyệt → dùng một lần), audit có đủ action và hash chain hợp lệ, logout.
- **preview** (`.github/workflows/preview.yml`, mỗi PR): build toàn bộ stack `docker compose` (Traefik + sandboxd + console), kiểm tra route qua Traefik, header CSP, network sandbox là `internal`, rồi chạy Playwright qua console thật và đính kèm ảnh chụp từng màn hình vào run.

Nguyên tắc: mỗi milestone phải xanh CI; test bằng Docker thật, không mock; luôn có bước cleanup.

## 11. Definition of Done

**Sau M5 (single-node):** toàn bộ API mục 4 dùng được bằng `curl`; GUI hoạt động đầy đủ; CI xanh; e2e cover login, image tuỳ chỉnh, policy clamp, keep-alive và delete; các kiểm soát #1 đến #7 và #9 đến #13 ở mục 8 có test; không còn route hay package agent.

**Sau M7 (distributed):** 2 worker (1 VPS Docker, 1 k3s pod) join cùng control plane; admin thấy cả 2 node, drain được, và thấy placement của từng sandbox; metrics hiển thị đúng trên cả 2 node.

## 12. Cấu trúc repo

```
4AiCoding/
├── PLAN.md                 ← văn bản này
├── README.md               ← (M2) viết lại
├── LICENSE                 ← MIT của sandboxd
├── control-plane/          ← code Go
├── console/                ← (M4) viết lại
├── image/                  ← base image sandbox (không còn runtimed/agent)
├── scripts/e2e.sh, scripts/e2e/Dockerfile
├── docker-compose.yml
├── docs/                   ← tài liệu cũ của 4AiCoding + sandboxd
└── .github/workflows/ci.yml
```
