# k8s-availability-reporter


k8s-availability-reporter 是一個專為 OpenShift Container Platform (OCP) 與 Kubernetes 叢集設計的輕量級命令列工具。它能自動掃描叢集內所有的工作負載 (Workloads)，綜合分析 Pod 的調度規則，並計算出各服務的「最高可用節點數」與「允許失效節點數」，最後匯出為易於閱讀的 Excel 報表。

## ✨ 核心功能

* **全面的規則解析**：綜合評估 `NodeSelector`, `Taint/Toleration`, `NodeAffinity` (RequiredDuringScheduling) 等靜態調度規則。
* **智慧容錯計算**：根據 Workload 類型自動推算允許的節點失效數量：
  * **Stateless (Deployment/ReplicaSet)**：支援解析 `Self Anti-Affinity`，精準計算分散部署時所需的最低節點數。
  * **Stateful (StatefulSet)**：自動偵測 `HostPath` 等本地儲存綁定，識別單點故障風險。
  * **DaemonSet**：依據可用節點數動態計算。
  * **Bare Pods**：精準標示無 Controller 管理的獨立 Pod，並將容錯率標記為 0。
* **風險警示標註**：自動偵測並標註與其他服務互斥 (Inter-Workload Anti-Affinity) 的潛在調度風險。
* **Excel 報表匯出**：自動產生 `.xlsx` 檔案，方便維運團隊進行容量規劃與高可用性 (HA) 稽核。

## 🚀 系統需求

* **Go**: 1.25 或以上版本 (編譯用)
* **Kubernetes/OCP 權限**: 執行環境需具備讀取 `Nodes`, `Pods`, `Deployments`, `StatefulSets`, `DaemonSets` 的 RBAC 權限。
* **連線設定**: 預設讀取本機的 `~/.kube/config`，也支援 In-Cluster 執行。

## 🛠️ 安裝與編譯

1. **複製專案並進入目錄**
   ```bash
   git clone https://your-git-repo-url/ocp-availability-reporter.git
   cd ocp-availability-reporter
   ```

2. **下載相依套件**
   本專案使用 `client-go` (v0.32.0) 與 `excelize` (v2)。
   ```bash
   go mod tidy
   ```

3. **編譯執行檔**
   ```bash
   go build -o ocp-report main.go
   ```
   *(註：若需跨平台編譯，可加上環境變數，例如 `GOOS=linux GOARCH=amd64 go build -o ocp-report main.go`)*

## 📖 使用方式

### 基本執行 (使用預設 kubeconfig)
直接執行編譯好的二進位檔，程式會自動尋找 `~/.kube/config` 進行連線：
```bash
./ocp-report
```

### 指定特定的 kubeconfig
若你需要掃描多個不同的叢集，可以透過 flag 指定設定檔路徑：
```bash
./ocp-report -kubeconfig=/path/to/custom/kubeconfig.yaml
```

## 📊 輸出報表說明

執行成功後，會在當前目錄下生成 `ocp_availability_report.xlsx`。報表包含以下欄位：

* **Namespace**: 工作負載所在的命名空間。
* **Workload Name**: 服務名稱。
* **Type**: 資源類型 (Deployment, StatefulSet, DaemonSet, Pod)。
* **Replicas**: 目前設定的副本數。
* **Eligible Nodes Count**: 依據靜態調度規則，目前叢集內「有資格」運行此 Pod 的總節點數。
* **Allowed Node Failures**: 在保證服務不完全中斷 (或符合高可用性設定) 的前提下，允許同時失效的節點數量。
* **Flags / Warnings**: 系統自動產生的警示，例如發現 `Self Anti-Affinity` 限制、綁定本地儲存，或是屬於高風險的獨立 Pod。

## 📝 注意事項

* 本工具為「靜態容量評估」，主要依據基礎架構與調度規則進行推算，並未即時監控節點的即時 CPU/Memory 剩餘資源。
* 針對 `StatefulSet`，目前的本地儲存檢查邏輯主要掃描 `HostPath`，若使用特定 CSI 驅動的 Local Volume，可能需依據實際環境調整過濾條件。
