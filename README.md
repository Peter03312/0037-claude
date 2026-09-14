# 制动分配阀整周期验算 API（brakealign）

对一份按递增毫秒采样的压力记录（CSV）与一份有序阶段规格（JSON：充气 / 保压 / 缓解），
做**整周期**对齐验算：枚举每个阶段的全部合法区段，用动态规划拼出覆盖首行到末行、
相邻阶段共享一个边界样本的完整切分，给出可追溯的合格 / 判废结论。

纯后端，Go 1.24 标准库 `net/http` 实现：**无前端、无数据库、无任何在线依赖**。

---

## 为什么不能逐段取首个局部匹配

> 制动分配阀的一段保压，开头像充气的尾部，结尾像缓解的开端。

若每阶段都从最早的合法右端贪心收尾，这个“局部首个匹配”可能恰好消耗掉后续阶段唯一
可用的边界样本，从而把**合格阀误判**。本服务不做贪心：

1. 对每个阶段、每个可能的起点，枚举其**所有**满足约束的右端；
2. 用动态规划在全部合法区段上拼接整周期；
3. 多解时依次按
   ① 各阶段终值绝对偏差之和最小；
   ② 结束边界向量字典序最小
   选取唯一结论；
4. 找不到覆盖全周期的切分时结论只能是 `rejected`，并按
   “已完成阶段最多、结束边界向量最小”给出诊断路径与下一阶段的违规区间，
   **绝不输出部分合格**。

## 切分语义

- 首阶段必须从 CSV **首行**开始，末阶段必须在**末行**结束；
- 相邻阶段**共享恰好一个边界样本**（前阶段末样本 == 后阶段首样本），其余样本只归属一个阶段；
- 因此各阶段时长（间隔数）之和恰为整周期间隔数，无重叠、无遗漏；
- 候选区段内任意相邻采样间隔超过该阶段 `max_sample_gap_ms` 时，该区段立即无效（采样断口）。

## 阶段约束

| 阶段 | 受控压力 | 反向累计量 | 特有约束 |
| --- | --- | --- | --- |
| `charging` 充气 | JSON 指定列 | 区间内**累计压力下降量** ≤ `allowed_reverse_cumulative` | — |
| `release` 缓解 | JSON 指定列 | 区间内**累计压力上升量** ≤ `allowed_reverse_cumulative` | — |
| `holding` 保压 | JSON 指定列 | 不适用（字段禁填） | 闭带越带时长，见下 |

所有阶段还共同受：闭区间时长 `[min_ms, max_ms]`、末样本受控压力相对
`target_final` 的绝对偏差 ≤ `abs_tolerance`、最大采样间隔。

判定一律为**严格数学语义，不设任何工程容差去放宽规格**：实测值恰好等于上限
（闭区间）判合格；超出上限任意小的数值（哪怕 1e-10）也判不合格。系统绝不会因为
“只超了一点点”而擅自返回合格。

### 保压闭带与分段线性插值

保压带为闭区间 `[target_final - holding_band_half_width, target_final + holding_band_half_width]`。
压力按**相邻样本分段线性**处理：越带判定不局限于样本点，而是求每段直线与上下界的
交点时刻（可为非整数毫秒），据此得到：

- 每一段**连续**越带的时长（跨样本点的越带只要中间没有严格回到带内，就合并为一次；
  只在界点相接也算连续；中间夹有严格带内区间则拆成两次）；
- 越带总时长。

要求单次连续越带 ≤ `max_single_out_of_band_ms`、越带总时长 ≤
`max_total_out_of_band_ms`。恰好用满预算（闭区间语义）判合格。

**从带边缘向外偏离不会被漏掉**：当某个样本点压力恰好落在界点上、随后向外偏离时，
端点状态取“越过该点之后的单侧极限”（看运动方向把它带到带内还是带外），因此界点
之后整段越带时间都被计入；只有界点这一个零宽时刻本身不算越带。反之，从界点回到
带内则不产生越带时间。

---

## 输入

`POST /verify`，`multipart/form-data`，两个文件部件：

### `csv` 部件

第一行表头，必须包含三列（列顺序任意，允许额外列）：

```csv
ms,train_pipe,brake_cylinder
0,50,300
100,80,300
```

- `ms`：非负整数毫秒，**严格递增**；
- `train_pipe`：列车管压力；`brake_cylinder`：制动缸压力；
- 压力必须是有限数（不接受 `NaN`/`Inf`）。

缺列、重复列、时间不递增、非有限数、行列数不一致等，**整单一次性**报带行列号的
错误（HTTP 400），不在脏数据上做任何对齐。**同一行上的多个问题会一次全部报出**
（例如某行时间格式非法且某压力列为 `NaN`，会同时给出两条带行列号的错误），
检修员修复后不会再次撞上此前未报告的问题。

### `spec` 部件

```json
{
  "phases": [
    {
      "kind": "charging",
      "controlled_column": "train_pipe",
      "duration_ms": {"min_ms": 200, "max_ms": 200},
      "target_final": 110,
      "abs_tolerance": 1,
      "allowed_reverse_cumulative": 0,
      "max_sample_gap_ms": 200
    },
    {
      "kind": "holding",
      "controlled_column": "train_pipe",
      "duration_ms": {"min_ms": 200, "max_ms": 200},
      "target_final": 100,
      "abs_tolerance": 5,
      "holding_band_half_width": 5,
      "max_single_out_of_band_ms": 50,
      "max_total_out_of_band_ms": 50,
      "max_sample_gap_ms": 200
    },
    {
      "kind": "release",
      "controlled_column": "train_pipe",
      "duration_ms": {"min_ms": 400, "max_ms": 400},
      "target_final": 40,
      "abs_tolerance": 1,
      "allowed_reverse_cumulative": 0,
      "max_sample_gap_ms": 200
    }
  ]
}
```

字段约束：未知字段拒收；`kind ∈ {charging, holding, release}`；
`controlled_column ∈ {train_pipe, brake_cylinder}`；三个越带字段仅保压阶段可填且必填，
`allowed_reverse_cumulative` 仅充气 / 缓解阶段可填且必填；时长、容差、预算均须为
有限数且非负，`max_ms ≥ min_ms`，带宽须为正。

## 输出

### 合格（HTTP 200）

`verdict = "qualified"`，逐阶段给出可追溯明细：样本序号、**CSV 行号**、毫秒时刻、
实际/允许时长、终值与目标及偏差；保压阶段附插值求得的单次/累计越带时长与预算；
并给出 `end_boundary_vector`（各阶段结束样本序号）与
`total_final_deviation`（终值偏差之和）。

### 判废（HTTP 200，`verdict = "rejected"`）

没有完整切分时不返回任何阶段结果（不给部分合格），而在 `diagnosis` 中给出：

- `completed_phases`：已完成阶段数（取最大值）；
- `end_boundary_vector`：已完成路径的结束边界向量（取字典序最小）；
- `next_phase_index` 与下一阶段起点（样本序号 / CSV 行号 / 毫秒）；
- `violation`：从该起点起**边界序最小**的违规区间（同样以样本/行号/毫秒三重坐标引用），
  含违规代码、实测值与预算；采样断口时另附逐段断口清单。

违规代码：`sample_gap`、`duration_out_of_range`、`final_value_deviation`、
`reverse_cumulative`、`single_excursion_too_long`、`total_excursion_too_long`。

### 输入错误（HTTP 400）

```json
{"status":"error","errors":[{"location":"row 4, column \"ms\"","message":"..."}]}
```

另有 `GET /healthz` 存活探针。

---

## 本地运行（需 Go 1.24）

```bash
go run ./cmd/api            # 默认 :8080，可用 PORT 改容器内监听端口

curl -s -F csv=@examples/record.csv -F spec=@examples/phases.json \
  http://localhost:8080/verify

# 判废示例（末值 35 偏离目标 40 超过容差 1）
curl -s -F csv=@examples/record_bad.csv -F spec=@examples/phases_bad.json \
  http://localhost:8080/verify
```

端到端冒烟（真实 multipart 请求与独立断言，期望值不参与服务端对齐）：

```bash
go run ./cmd/smoke -url http://localhost:8080
```

测试：

```bash
go test -v ./...
```

覆盖：共享边界、分段线性插值越带（整数 / 非整数交点、跨样本点合并与拆分、
恰好用满预算、**从带边缘向外偏离不漏报**）、局部贪心失败（枚举 + DP 找回合格阀）、
采样断口、偏差和与字典序两级择优（**微小偏差差异也选偏差最小者，不被容差吞掉**）、
**终值仅超极小量仍判不合格**、拒判诊断路径、缺列 / 时间不递增 / 非有限数整单报错、
**同一行时间与压力多问题一次报全**、HTTP 层。

## Docker Compose

```bash
# 启动常驻 API（宿主端口默认 8080）
docker compose up api

# 改宿主映射端口
API_PORT=9090 docker compose up api

# 一次性 verify 服务：等待 api 健康后执行 go test 全套单测与 HTTP 冒烟，随后退出
docker compose up --build verify
```

- `api`：常驻验算服务，容器内固定监听 8080，宿主映射由 `API_PORT`（默认 8080）控制；
- `verify`：一次性服务（`restart: "no"`），跑通全部检查后以 0 退出，失败以非零退出，
  便于在检修流水线中直接作为可追溯的验收闸门。

## 项目结构

```
cmd/api/main.go      HTTP 服务入口（PORT）
cmd/smoke/main.go    端到端 HTTP 冒烟
spec.go              阶段规格解析与整单校验
dataset.go           CSV 解析与行列错误收集
eval.go              反向累计、采样断口、保压分段线性越带几何、候选枚举
align.go             枚举 + 动态规划、两级择优、判废诊断路径
server.go            net/http Handler、multipart 入参
examples/            合格 / 判废示例单据
Dockerfile / Dockerfile.verify / docker-compose.yml
```
