# Quy trình cho Z agent (z.ai coding agents)

File này là **nguồn duy nhất** cho cách Z agent nhận và làm việc trong repo. Đọc lại nó ở đầu mỗi lượt chạy, vì reviewer có thể cập nhật bất cứ lúc nào.

Mục tiêu sản phẩm: người dùng vào https://chat.devhub.io.vn → landing → đăng nhập/đăng ký → Chat (và Agent làm việc trong sandbox) chạy được.

## Bạn là ai (đọc trước tiên)

Tên của bạn (**Z1** hoặc **Z2**) chỉ lấy từ **dòng đầu tiên trong prompt của bạn**. Không suy ra tên từ lịch sử repo, từ commit, từ comment hay từ issue cũ. Cả hai agent dùng chung một tài khoản GitHub, nên những thứ đó không cho biết bạn là ai.

Mỗi lượt chạy, trước khi làm bất cứ việc gì:
1. Đọc lại dòng đầu prompt và ghi nhớ `AGENT` (Z1 hoặc Z2) cùng `LABEL` (z1 hoặc z2).
2. Đặt danh tính cho git: `git config user.name "<AGENT> Agent"`, `git config user.email "kungfufanta@users.noreply.github.com"`.
3. Mọi dấu vết bạn để lại phải mang tên của bạn:
   - Branch: `zai/<LABEL>-<số issue>-<slug>`
   - Tiêu đề PR: `<AGENT>: …`
   - **Mọi** comment trên issue hoặc PR mở đầu bằng `[<AGENT>]`, ví dụ `[Z1] Ready for re-review`.
4. Chỉ đụng vào issue có nhãn `LABEL` của mình, và PR hoặc branch có tiền tố `zai/<LABEL>-` của mình. Thấy PR hoặc branch của agent kia thì bỏ qua, kể cả khi nó có review `[CHANGES REQUESTED]`.

Nếu prompt của bạn không ghi rõ Z1 hay Z2 thì dừng lượt đó và không làm gì.

## Agent và phạm vi

| Agent | Nhãn | Phạm vi file chính |
|---|---|---|
| Z1 | `z1` | `portal/web/**`, `portal/e2e/**` (frontend + browser test) |
| Z2 | `z2` | `portal/internal/**`, `portal/migrations/**`, `portal/cmd/**`, `deploy/**`, `scripts/**`, `.github/workflows/**` (chỉ khi issue cho phép) |

Mỗi issue có mục **"Phạm vi file"**. Mục đó mới là giới hạn chính xác.

Nhãn:
- `zai`: issue dành cho Z agent.
- `z1` / `z2`: agent nào nhận.
- `P0` / `P1` / `P2`: ưu tiên.
- `blocked`: đang chờ issue khác, chưa được nhận.
- `in-progress`: đã có agent nhận.
- `doan`: việc của Doan, Z agent không nhận.

## Mỗi lượt chạy (1 giờ/lần), làm đúng thứ tự

### 0. Đọc trạng thái
- `git fetch origin` và làm việc trên `origin/main` mới nhất.
- Liệt kê PR đang mở của mình (branch `zai/<nhãn của mình>-*`).
- Nếu một PR của mình có CI đỏ, hoặc có review/comment của reviewer bắt đầu bằng **`[CHANGES REQUESTED]`** mà chưa xử lý: sửa PR đó **trước** (push thêm commit lên cùng branch), comment `ready for re-review` trên PR, rồi kết thúc lượt. Không nhận issue mới. (Reviewer và Z agent dùng chung tài khoản GitHub nên GitHub không cho bấm "Request changes"; tiền tố `[CHANGES REQUESTED]` thay cho trạng thái đó.)
- Reviewer có thể tự đẩy commit lên branch của bạn (ví dụ merge main để gỡ conflict). Luôn `git pull` branch của mình trước khi sửa tiếp.
- Nếu PR conflict với main: rebase lên `origin/main`, chạy lại test, rồi `git push --force-with-lease`. Chỉ làm vậy với branch của chính mình.

### 1. Chọn issue
- Chọn issue OPEN có nhãn `zai` **và** nhãn của mình, **không** có `blocked`, **không** có `in-progress`.
- Ưu tiên P0 > P1 > P2. Cùng mức thì số issue nhỏ hơn làm trước.
- **Tuyệt đối không** nhận issue có nhãn của agent kia hoặc nhãn `doan`, và không sửa file thuộc phạm vi của agent kia.
- Hết issue phù hợp: comment một lần mỗi ngày vào issue #89 "`<Z1|Z2>`: hết việc", rồi kết thúc lượt.

### 2. Nhận việc
- Gắn nhãn `in-progress` cho issue và comment: `🔒 <Z1|Z2> nhận issue này, branch zai/<nhãn>-<số issue>-<slug>`.
- Mỗi lúc chỉ giữ tối đa **1** issue `in-progress` chưa có PR.

### 3. Làm
- Branch: `zai/<nhãn>-<số issue>-<slug ngắn>`, tạo từ `origin/main`.
- Chỉ sửa file nằm trong "Phạm vi file" của issue. Nếu cần sửa ngoài phạm vi, comment vào issue để giải thích, không tự làm.
- Làm đủ mọi ô trong "Acceptance criteria". Mọi thay đổi Go phải có test.
- Frontend: trong `portal/web` chạy `corepack enable && pnpm install --frozen-lockfile && pnpm build`.
- Go: chạy `gofmt`, `go vet ./...` và `go test -race ./...` trong module mình sửa (`portal/` hoặc `control-plane/`).
- **Không** commit file build. `portal/internal/webfs/dist/*` phải giữ nguyên placeholder. Nếu lỡ build ra thì chạy `git checkout -- portal/internal/webfs/dist`.
- **Không** commit secret, API key, mật khẩu, token hay URL có kèm credential. Repo này là **public**. Dùng biến env hoặc placeholder.
- **Không** sửa `.github/workflows` trừ khi issue cho phép rõ ràng. **Không** tắt, skip hay xoá test để CI xanh.
- Commit nhỏ, message bằng tiếng Anh, rõ ràng.

### 4. Mở PR
- Tiêu đề: `<Z1|Z2>: <tiêu đề issue, bỏ phần [P..][Z..]>`.
- Dòng đầu của body là `Closes #<số issue>`. Sau đó là danh sách acceptance criteria đã tick, cách test, và ảnh chụp nếu là thay đổi UI.
- Base: `main`. **Không tự merge**: reviewer (Claude) review và merge mỗi 3 giờ.
- Đợi CI chạy. Nếu CI đỏ, đọc log và sửa ngay trong lượt nếu kịp. Nếu nghi test bị flake (mạng, DNS, apt, test chậm), ghi tên test và link log vào comment PR. **Không** đẩy commit rỗng (`ci: retrigger`) để chạy lại CI.

### 5. Kết thúc lượt
- Comment tiến độ ngắn vào issue: link PR, CI xanh hay đỏ.
- Không để quá **2** PR chưa merge cùng lúc. Khi đã có 2 PR chờ review thì chỉ sửa theo review, không nhận issue mới.

## Tài liệu tham chiếu
- `docs/AGENT_PLAN_A3.md`: agent loop trong Chat.
- `docs/PLATFORM_RLS_PLAN.md`: RLS, org nền tảng, các phase R0–R5.
- `docs/PORTAL_PLAN_V2.md`, `portal/README.md`.

Ngôn ngữ: chữ trên giao diện bằng tiếng Việt. Code, commit và tên biến bằng tiếng Anh.

## Prompt để dán vào Z agent

Mỗi agent dán đúng prompt của mình. Hai prompt khác nhau ở dòng đầu và ở nhãn.

**Agent thứ nhất (frontend):**
```text
AGENT=Z1 LABEL=z1. Bạn là Z1, agent frontend (portal/web, portal/e2e) trên repo kungfufanta/4AiCoding.
Mỗi lượt: git fetch origin, đọc docs/ZAI_AGENT.md trên origin/main và làm đúng theo đó với tư cách Z1.
Chỉ nhận issue có nhãn z1, chỉ sửa PR hoặc branch zai/z1-*. Mọi comment mở đầu bằng [Z1]. Không bao giờ commit secret.
```

**Agent thứ hai (backend):**
```text
AGENT=Z2 LABEL=z2. Bạn là Z2, agent backend (portal/internal, migrations, deploy, workflows) trên repo kungfufanta/4AiCoding.
Mỗi lượt: git fetch origin, đọc docs/ZAI_AGENT.md trên origin/main và làm đúng theo đó với tư cách Z2.
Chỉ nhận issue có nhãn z2, chỉ sửa PR hoặc branch zai/z2-*. Mọi comment mở đầu bằng [Z2]. Không bao giờ commit secret.
```
