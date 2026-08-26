# 社團組織架構與 mailing list

本文件說明 NYCU SDC 的組織架構如何對應到 Google Workspace 的 mailing list，以及這個服務怎麼使用這些資料。

現況數字為 **2026-08-25** 的快照。**真相永遠在 Google**，本文件只是導覽 —— 讀到跟實際不符時，是文件過期了，不是 Google 錯了。

## 一、三層分離

這是理解整份文件的前提。三件事各有各的真相來源，不要混在一起：

| 層 | 真相在哪 | 決定什麼 | 改動方式 |
|---|---|---|---|
| **階層** | Google 的巢狀 group | 誰實際收得到信 | Workspace admin console |
| **顯示** | `internal/orgchart` 的設定 | 顯示名稱、分類、排序、角色歸屬 | 發 PR |
| **服務行為** | `config.yaml` 的 `google_group` | 誰能登入、誰是 admin | 改設定 + 重啟 |

換句話說：**在 admin console 把一個 group 加進另一個 group，就是在改組織架構**，不需要動程式。反過來，改 `internal/orgchart` 只影響畫面上的名稱與順序，不會讓任何人多收到或少收到一封信。

## 二、階層現況

`X` 底下列 `Y`，表示 **Y 是 X 的成員** —— 也就是寄給 `X@` 的信，`Y` 裡的所有人都收得到。X 是超集。

```
general (80)                      ← 全社；本服務的登入閘門
├─ committee (13)                 ← 幹部會
│  ├─ administration (19)
│  ├─ branding (16)
│  │  └─ design (11)
│  ├─ consultants (36)
│  ├─ education (17)
│  ├─ engineering (19)
│  └─ finance (11)
├─ clustron (14)
├─ core-system (23)
├─ design (11)                    ← 同時直接掛在 general 底下
├─ hpc (24)
│  ├─ hpc-apac2026 (7)
│  └─ polaris (6)
├─ hpc-training (31)
│  └─ hpc (24)                    ← hpc-training 是 hpc 的超集
├─ itsc-hr (13)
├─ project-training (20)
├─ sciedu-llm (14)
├─ sre (13)
└─ welcome-home (42)
```

注意這是 **DAG 不是樹**：`design` 有兩個父節點（`branding` 與 `general`），`hpc` 也有兩個（`general` 與 `hpc-training`）。任何走訪這張圖的程式都必須帶 visited set。

## 三、全部 34 個 mailing list

顯示名稱為 `internal/orgchart/chart.yaml` 裡的英文名（拿掉 Google 上的 `NYCU SDC ` 前綴）。下表的順序就是 API 回傳的順序。

### All Members

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `general` | 80 | General。全社名單，**本服務的登入閘門**，只有這裡的直接成員能登入 |

### Governance

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `presidents` | 7 | Presidents。社長與五位副社長 + `admin@` |
| `committee` | 13 | Committee。成員包含五個部門 group 與 `consultants` |
| `consultants` | 36 | Consultants |

### Departments

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `administration` | 19 | Administration |
| `branding` | 16 | Branding。成員包含 `design` |
| `design` | 11 | Design Team |
| `education` | 17 | Education |
| `engineering` | 19 | Engineering |
| `finance` | 11 | Finance |

### Infra Team

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `infrastructure` | 13 | Infrastructure Committee |

### Project Team

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `core-system` | 23 | Core System |
| `clustron` | 14 | Clustron |
| `sciedu-llm` | 14 | Edu Institute Collab（與 NYCU 教育所合作） |
| `itsc-hr` | 13 | IT Center Collab（與 NYCU 資訊技術服務中心合作） |
| `sre` | 13 | SRE Team |

### HPC Team

各個比賽的團隊。

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `hpc` | 24 | HPC Team。成員包含 `hpc-apac2026` 與 `polaris` |
| `hpc-apac2026` | 7 | APAC 2026 |
| `hpc-hipac-2026` | 18 | HiPAC 2026 |
| `polaris` | 6 | Polaris |

### Training

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `hpc-training` | 31 | HPC Training。成員包含 `hpc` |
| `project-training` | 20 | Project Training |

### Program

名稱裡帶 `program` 的課程名單。

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `full-stack-intro-training-program-fall-2025` | 39 | Full Stack Intro. Fall 2025 |
| `full-stack-advanced-training-program-fall-2025` | 23 | Full Stack Advanced Fall 2025 |
| `full-stack-intro-training-program-spring-2026` | 26 | Full Stack Intro. Spring 2026 |
| `full-stack-advanced-training-program-spring-2026` | 18 | Full Stack Advanced Spring 2026 |
| `gpu-programming-training-program-spring-2026` | 17 | GPU Programming Spring 2026 |
| `react-credit-program-spring-2026` | 36 | React Credit Spring 2026 |

### Events

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `welcome-home` | 42 | Welcome Home |
| `wch-finance` | 12 | Welcome Home Finance |
| `world-riscv-days` | 38 | World RISC-V Days |
| `ed312` | 27 | ED312 Remote Lab |

### System（個人頁面不顯示）

`GET /api/groups` 仍會列出這一區——那是整個帳號的清單，藏起來只會讓人以為 group 不見了。

| group | 人數 | 顯示名稱 |
|---|---:|---|
| `info` | 3 | Mail Backup。成員為 `committee` 與 `consultants` |
| `classroom_teachers` | 0 | Classroom Teachers。**Google Workspace 內建，不可刪除** |

## 四、角色

| 角色 | 負責的 group |
|---|---|
| 社長 | 統籌，對應 `committee` |
| 執行副社長 | `administration`、`finance`、`branding`（含 `design`） |
| 技術副社長 (infra) | `infrastructure` |
| 超算副社長 (HPC) | `hpc` |
| 教務副社長 | `education` |
| 開發副社長 | `engineering` |

`presidents` 目前是 6 人 + `admin@`，數量與上表吻合。

**為什麼角色要寫在 repo 而不是從 Google 推導**：這六位在**每一個**部門 group 都是 `MANAGER`。所以 Google 的成員 role 只能回答「這個人是不是幹部」，回答不了「這個人負責哪個部門」。想從 `MANAGER` 推出負責範圍是行不通的。

這也是本服務 `admin` 權限的來源：`internal/auth.localRoleFor` 把 `login_group`（即 `general`）裡的 `OWNER`/`MANAGER` 映成 `admin`，其餘為 `member`。詳見 CLAUDE.md 的 Auth 段落。

## 五、已知問題

### 5.1 有 14 個 group 不在 `general` 底下

`infrastructure`、`presidents`、`ed312`、`world-riscv-days`、`wch-finance`、`hpc-hipac-2026`、六個 training program、`classroom_teachers`、`info`。

這代表：寄給 `general@` 的全社公告，這些 group 的成員**不會**因為身在該 group 而收到。

### 5.2 36 位核心團隊成員無法登入本服務

登入閘門檢查的是 `general` 的**直接**成員（`members.list` 不遞迴，見 CLAUDE.md）。全部 group 累計出現 214 人，`general` 直接成員只有 69 人，因此有 145 人登入會拿到 `#error=not_a_member`。

其中大多數是課程學員（training program、`ed312`），本來就不該有帳號。但**有 36 人屬於核心團隊**：

| group | 登不進來 / 直接成員 |
|---|---|
| `hpc` | 14 / 24 |
| `core-system` | 6 / 23 |
| `administration` | 5 / 19 |
| `infrastructure` | 5 / 13 |
| `branding` | 4 / 16 |
| `clustron` | 4 / 14 |
| `engineering` | 4 / 19 |
| `education` | 4 / 17 |
| `design` | 3 / 11 |
| `polaris` | 3 / 6 |
| `sre` | 3 / 13 |
| `finance` | 1 / 11 |

這也是為什麼 `GET /api/users` 的名冊只有 69 人：它讀的就是 `login_group` 的直接成員，跟登入閘門同一份名單。這 36 個人登不進來，也不會出現在名冊上——同一個根因的兩個症狀。

**這是幹部要處理的事，不是程式該繞過的。** 現在可以用 `POST /api/users`（帶 email）逐一補進來，不必進 admin console。 把這些人加進 `general` 的直接成員即可（把部門 group 掛進 `general` 沒有用 —— 不遞迴）。

放寬登入閘門去接受間接成員是個壞主意：`general` 底下掛著 `hpc-training`、`project-training` 等課程名單，遞迴會讓所有修過課的人都能登入。

### 5.3 `groups.list?userKey=` 只回直接成員資格

已於 2026-08-25 實測確認：對只在 `design`、不在 `branding` 直接名單的 `tsaozero.work@gmail.com` 查詢，只回傳 `core-system`、`design`、`world-riscv-days`，沒有 `branding`、`committee`、`general`。

所以「使用者所屬的 mailing list」若要顯示間接關係，**必須自己往上展開**，不能指望 Google 代勞。

**不過本服務刻意不展開** —— 見 5.5。

### 5.4 但 `userKey` 也吃 group 的地址

同日實測發現的第二件事：`userKey` 雖然文件上寫「使用者」，實際上傳 group 的地址一樣有效，回傳的是**該 group 的直接父節點**：

```
groups.list?userKey=design@sdc.nycu.club    -> branding, general
groups.list?userKey=branding@sdc.nycu.club  -> committee
groups.list?userKey=hpc@sdc.nycu.club       -> general, hpc-training
```

（`design` 的祖父 `committee` 沒有出現，所以確實是直接父節點而非遞迴。）

這讓「往上展開」可以一層一層懶惰查詢，不必為了知道階層而把整個帳號的巢狀關係建出來 —— 後者要對 34 個 group 各打一次 `members.list`，實測 15.7 秒。**這個行為在 Google 的文件裡找不到**，所以記在這裡。

本服務目前沒有用到這個技巧（見 5.5），但它是實測得來的知識，留著。

### 5.5 API 刻意只回報直接成員資格

`/api/users/me/groups`、`/api/users/me` 的 `groups` 與名冊的 `groups` 都**只列出這個人真的被加進去的名單**，不往上展開巢狀關係。

理由是實測出來的：對名冊上的 69 人展開之後，**52 人會多出 `committee`**，因為 `consultants`（36 人）是 `committee` 的成員。以「信會不會寄到」來說沒錯，以「這個人在組織裡的位置」來說是錯的 —— 顧問不是幹部會成員。展開帶來的資訊裡絕大多數是這種誤導。

第二節的巢狀樹描述的是帳號的**真實結構**，那跟 API 回報什麼是兩回事，兩者都對。

## 六、新增 mailing list 時的檢查清單

1. **要不要掛進 `general`？** 不掛的話，成員收不到全社公告。注意這**不會**讓他們能登入 —— 登入需要個別加進 `general` 的直接成員。
2. **要不要掛進 `committee`？** 只有部門層級才需要。
3. **`internal/orgchart` 的設定要補一行**：`groups` 補英文顯示名稱（拿掉 `NYCU SDC ` 前綴），`sections` 決定它出現在哪一區、排第幾。沒補的話會落到「未分類」並在 log 出現 warn —— 會被看見，不會靜默消失。
4. **是課程名單嗎？** 課程名單成員通常不該有本服務的帳號，不要為了讓他們登入而把他們加進 `general`。

## 七、給開發者

- 階層來自 Google，**不要在 repo 裡另外維護一份會漂移的階層定義**。`internal/orgchart` 只放顯示資訊。
- 巢狀圖是 DAG，走訪時務必帶 visited set —— Google 不禁止 group 互相成為對方的成員。
- API 對外一律使用短名稱（`general` 而非 `general@sdc.nycu.club`），由 `google_group.domain` 控制。成員的 email 不受影響。詳見 CLAUDE.md 的 Google mailing lists 段落。
