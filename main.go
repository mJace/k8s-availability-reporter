package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"path/filepath"
	"strings"

	"github.com/xuri/excelize/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
)

// ReportItem 儲存單一工作負載的可用性報告結果
type ReportItem struct {
	Namespace           string
	WorkloadName        string
	Type                string
	Replicas            int32
	EligibleNodeCount   int
	EligibleNodes       []string
	AllowedNodeFailures int
	PVAccessModes       string
	PriorityClass       string
	HasResourceRequests bool
	HasProbes           bool
	PDBStatus           string
	Flags               []string
}

// WorkloadSummary 作為中介結構，統一不同資源類型的屬性
type WorkloadSummary struct {
	Namespace string
	Name      string
	Kind      string
	Replicas  int32
	Labels    map[string]string
	PodSpec   corev1.PodSpec
	Priority  string
	HasReq    bool
	HasProbe  bool
	PVModes   string
	PDBInfo   string
}

func main() {
	// 1. 初始化 Kubernetes Client
	config, err := getKubeConfig()
	if err != nil {
		log.Fatalf("取得 kubeconfig 失敗: %v", err)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Fatalf("建立 clientset 失敗: %v", err)
	}

	ctx := context.TODO()

	// 2. 抓取叢集內所有 Node
	log.Println("正在抓取 Nodes...")
	nodesObj, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Fatalf("抓取 Nodes 失敗: %v", err)
	}
	log.Printf("成功抓取 %d 個 Nodes", len(nodesObj.Items))

	// 2-1. 預先抓取所有 PVC
	log.Println("正在抓取 PersistentVolumeClaims...")
	pvcs, err := clientset.CoreV1().PersistentVolumeClaims("").List(ctx, metav1.ListOptions{})
	pvcMap := make(map[string]corev1.PersistentVolumeClaim)
	if err == nil {
		for _, pvc := range pvcs.Items {
			pvcMap[fmt.Sprintf("%s/%s", pvc.Namespace, pvc.Name)] = pvc
		}
	}

	// 2-2. 預先抓取所有 PDB
	log.Println("正在抓取 PodDisruptionBudgets...")
	pdbs, err := clientset.PolicyV1().PodDisruptionBudgets("").List(ctx, metav1.ListOptions{})
	pdbList := []policyv1.PodDisruptionBudget{}
	if err == nil {
		pdbList = pdbs.Items
	}

	// 3. 抓取所有 Namespace 的 Workloads (Deployments, StatefulSets, DaemonSets)
	log.Println("正在掃描所有 Namespace 的 Workloads...")
	var workloads []WorkloadSummary

	// 3-1. Deployments
	deps, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("抓取 Deployments 失敗: %v", err)
	} else {
		log.Printf("正在處理 %d 個 Deployments...", len(deps.Items))
		for _, d := range deps.Items {
			var reps int32 = 1
			if d.Spec.Replicas != nil {
				reps = *d.Spec.Replicas
			}
			workloads = append(workloads, WorkloadSummary{
				Namespace: d.Namespace,
				Name:      d.Name,
				Kind:      "Deployment",
				Replicas:  reps,
				Labels:    d.Spec.Template.Labels,
				PodSpec:   d.Spec.Template.Spec,
				Priority:  d.Spec.Template.Spec.PriorityClassName,
				HasReq:    checkResourceRequests(d.Spec.Template.Spec),
				HasProbe:  checkProbes(d.Spec.Template.Spec),
				PVModes:   extractPVModes(d.Namespace, d.Spec.Template.Spec, pvcMap, nil),
				PDBInfo:   findMatchingPDB(d.Namespace, d.Spec.Template.Labels, pdbList),
			})
		}
	}

	// 3-2. StatefulSets
	sts, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("抓取 StatefulSets 失敗: %v", err)
	} else {
		log.Printf("正在處理 %d 個 StatefulSets...", len(sts.Items))
		for _, s := range sts.Items {
			var reps int32 = 1
			if s.Spec.Replicas != nil {
				reps = *s.Spec.Replicas
			}
			workloads = append(workloads, WorkloadSummary{
				Namespace: s.Namespace,
				Name:      s.Name,
				Kind:      "StatefulSet",
				Replicas:  reps,
				Labels:    s.Spec.Template.Labels,
				PodSpec:   s.Spec.Template.Spec,
				Priority:  s.Spec.Template.Spec.PriorityClassName,
				HasReq:    checkResourceRequests(s.Spec.Template.Spec),
				HasProbe:  checkProbes(s.Spec.Template.Spec),
				PVModes:   extractPVModes(s.Namespace, s.Spec.Template.Spec, pvcMap, s.Spec.VolumeClaimTemplates),
				PDBInfo:   findMatchingPDB(s.Namespace, s.Spec.Template.Labels, pdbList),
			})
		}
	}

	// 3-3. DaemonSets
	dsets, err := clientset.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("抓取 DaemonSets 失敗: %v", err)
	} else {
		log.Printf("正在處理 %d 個 DaemonSets...", len(dsets.Items))
		for _, ds := range dsets.Items {
			workloads = append(workloads, WorkloadSummary{
				Namespace: ds.Namespace,
				Name:      ds.Name,
				Kind:      "DaemonSet",
				Replicas:  0, // DaemonSet 的副本數隨節點變動，此處不適用固定數字
				Labels:    ds.Spec.Template.Labels,
				PodSpec:   ds.Spec.Template.Spec,
				Priority:  ds.Spec.Template.Spec.PriorityClassName,
				HasReq:    checkResourceRequests(ds.Spec.Template.Spec),
				HasProbe:  checkProbes(ds.Spec.Template.Spec),
				PVModes:   extractPVModes(ds.Namespace, ds.Spec.Template.Spec, pvcMap, nil),
				PDBInfo:   findMatchingPDB(ds.Namespace, ds.Spec.Template.Labels, pdbList),
			})
		}
	}

	// 3-4. Standalone Pods (無 Controller 管理的獨立 Pod)
	log.Println("正在掃描獨立的 Bare Pods...")
	podsObj, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err != nil {
		log.Printf("抓取 Pods 失敗: %v", err)
	} else {
		log.Printf("正在篩選 %d 個 Pods...", len(podsObj.Items))
		for _, p := range podsObj.Items {
			// Static Pods (Mirror Pods) 通常帶有特定的 annotation
			isStatic := p.Annotations["kubernetes.io/config.source"] != "" || p.Annotations["kubernetes.io/config.mirror"] != ""

			// 篩選條件：
			// 1. 沒有 OwnerReferences (Bare Pod) 或 它是 Static Pod
			// 2. 排除已經結束的狀態 (Succeeded/Failed)
			if (len(p.OwnerReferences) == 0 || isStatic) && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				kind := "Pod"
				if isStatic {
					kind = "Static Pod"
				}
				workloads = append(workloads, WorkloadSummary{
					Namespace: p.Namespace,
					Name:      p.Name,
					Kind:      kind,
					Replicas:  1,
					Labels:    p.Labels,
					PodSpec:   p.Spec,
					Priority:  p.Spec.PriorityClassName,
					HasReq:    checkResourceRequests(p.Spec),
					HasProbe:  checkProbes(p.Spec),
					PVModes:   extractPVModes(p.Namespace, p.Spec, pvcMap, nil),
					PDBInfo:   findMatchingPDB(p.Namespace, p.Labels, pdbList),
				})
			}
		}
	}

	// 4. 產生報告
	log.Println("開始分析可用性與容錯機制...")
	reports := GenerateOcpAvailabilityReport(nodesObj.Items, workloads)

	// 5. 輸出報告 (終端機格式化輸出)
	PrintReport(reports)

	fileName := "ocp_availability_report.xlsx"
	err = ExportToExcel(reports, fileName)
	if err != nil {
		log.Printf("匯出 Excel 失敗: %v", err)
	} else {
		log.Printf("成功匯出報告至: %s", fileName)
	}
}

// checkResourceRequests 檢查是否所有容器都有設定 Resources.Requests
func checkResourceRequests(spec corev1.PodSpec) bool {
	for _, container := range spec.Containers {
		if container.Resources.Requests.Cpu().IsZero() || container.Resources.Requests.Memory().IsZero() {
			return false
		}
	}
	return true
}

// checkProbes 檢查是否至少有定義 Liveness 或 Readiness Probe
func checkProbes(spec corev1.PodSpec) bool {
	for _, container := range spec.Containers {
		if container.ReadinessProbe != nil || container.LivenessProbe != nil {
			return true
		}
	}
	return false
}

// findMatchingPDB 尋找與工作負載標籤匹配的 PDB
func findMatchingPDB(ns string, wlLabels map[string]string, pdbs []policyv1.PodDisruptionBudget) string {
	for _, pdb := range pdbs {
		if pdb.Namespace != ns {
			continue
		}
		selector, err := metav1.LabelSelectorAsSelector(pdb.Spec.Selector)
		if err == nil && selector.Matches(labels.Set(wlLabels)) {
			return fmt.Sprintf("MinAvail: %v, MaxUnavail: %v", pdb.Spec.MinAvailable, pdb.Spec.MaxUnavailable)
		}
	}
	return "None"
}

// extractPVModes 從 PodSpec 與 VolumeClaimTemplates 中提取 PV 的 AccessModes
func extractPVModes(ns string, spec corev1.PodSpec, pvcMap map[string]corev1.PersistentVolumeClaim, vct []corev1.PersistentVolumeClaim) string {
	modes := make(map[string]bool)

	// 檢查 PodSpec 中的 Volumes
	for _, vol := range spec.Volumes {
		if vol.PersistentVolumeClaim != nil {
			key := fmt.Sprintf("%s/%s", ns, vol.PersistentVolumeClaim.ClaimName)
			if pvc, ok := pvcMap[key]; ok {
				for _, m := range pvc.Spec.AccessModes {
					modes[string(m)] = true
				}
			}
		}
	}

	// 針對 StatefulSet 的 VolumeClaimTemplates
	for _, pvcTmpl := range vct {
		for _, m := range pvcTmpl.Spec.AccessModes {
			modes[string(m)] = true
		}
	}

	if len(modes) == 0 {
		return "None"
	}
	var res []string
	for m := range modes {
		res = append(res, m)
	}
	return strings.Join(res, ", ")
}

// ExportToExcel 實作匯出邏輯
func ExportToExcel(reports []ReportItem, filename string) error {
	f := excelize.NewFile()
	sheetName := "Availability Report"
	f.SetSheetName("Sheet1", sheetName)

	// 設定表頭
	headers := []string{"Namespace", "Workload Name", "Type", "Replicas", "Priority", "Resources", "Probes", "PV Access Modes", "PDB Status", "Eligible Nodes Count", "Allowed Node Failures", "Flags / Warnings"}
	for i, header := range headers {
		cell, _ := excelize.CoordinatesToCellName(i+1, 1)
		f.SetCellValue(sheetName, cell, header)
	}

	// 寫入資料
	for i, r := range reports {
		row := i + 2
		f.SetCellValue(sheetName, fmt.Sprintf("A%d", row), r.Namespace)
		f.SetCellValue(sheetName, fmt.Sprintf("B%d", row), r.WorkloadName)
		f.SetCellValue(sheetName, fmt.Sprintf("C%d", row), r.Type)
		f.SetCellValue(sheetName, fmt.Sprintf("D%d", row), r.Replicas)
		f.SetCellValue(sheetName, fmt.Sprintf("E%d", row), r.PriorityClass)
		f.SetCellValue(sheetName, fmt.Sprintf("F%d", row), map[bool]string{true: "Defined", false: "MISSING"}[r.HasResourceRequests])
		f.SetCellValue(sheetName, fmt.Sprintf("G%d", row), map[bool]string{true: "Configured", false: "NONE"}[r.HasProbes])
		f.SetCellValue(sheetName, fmt.Sprintf("H%d", row), r.PVAccessModes)
		f.SetCellValue(sheetName, fmt.Sprintf("I%d", row), r.PDBStatus)
		f.SetCellValue(sheetName, fmt.Sprintf("J%d", row), r.EligibleNodeCount)
		f.SetCellValue(sheetName, fmt.Sprintf("K%d", row), r.AllowedNodeFailures)
		f.SetCellValue(sheetName, fmt.Sprintf("L%d", row), strings.Join(r.Flags, " | "))
	}

	// 套用基本的樣式 (加粗表頭)
	style, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#E0E0E0"}, Pattern: 1},
	})
	f.SetCellStyle(sheetName, "A1", "L1", style)

	return f.SaveAs(filename)
}

// 取得 kubeconfig，支援 In-Cluster (跑在 Pod 內) 或 Out-Of-Cluster (本機執行)
func getKubeConfig() (*rest.Config, error) {
	// 先嘗試使用 InClusterConfig
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}

	// fallback 到本機 ~/.kube/config
	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	return clientcmd.BuildConfigFromFlags("", *kubeconfig)
}

// ============== 核心分析邏輯 ==============

func GenerateOcpAvailabilityReport(nodes []corev1.Node, workloads []WorkloadSummary) []ReportItem {
	var report []ReportItem

	for _, wl := range workloads {
		var eligibleNodes []string

		for _, node := range nodes {
			if IsNodeLogicallyEligible(wl.PodSpec, node) {
				eligibleNodes = append(eligibleNodes, node.Name)
			}
		}

		eligibleNodeCount := len(eligibleNodes)
		hasSelfAntiAffinity := CheckSelfAntiAffinity(wl.PodSpec, wl.Labels)
		hasInterWorkloadAntiAffinity := CheckInterWorkloadAntiAffinity(wl.PodSpec, wl.Labels)
		allowedFailures := CalculateAllowedFailures(eligibleNodeCount, wl.Replicas, wl.Kind, wl.PodSpec, hasSelfAntiAffinity)

		item := ReportItem{
			Namespace:           wl.Namespace,
			WorkloadName:        wl.Name,
			Type:                wl.Kind,
			Replicas:            wl.Replicas,
			EligibleNodeCount:   eligibleNodeCount,
			EligibleNodes:       eligibleNodes,
			AllowedNodeFailures: allowedFailures,
			PVAccessModes:       wl.PVModes,
			PriorityClass:       wl.Priority,
			HasResourceRequests: wl.HasReq,
			HasProbes:           wl.HasProbe,
			PDBStatus:           wl.PDBInfo,
			Flags:               []string{},
		}

		if hasSelfAntiAffinity {
			item.Flags = append(item.Flags, fmt.Sprintf("【注意】具備 Self Anti-Affinity，至少需 %d 台可用節點", wl.Replicas))
		}
		if hasInterWorkloadAntiAffinity {
			item.Flags = append(item.Flags, "【風險】設有 Inter-Workload Anti-Affinity，受其他服務位置影響")
		}
		if wl.Kind == "Pod" || wl.Kind == "Static Pod" {
			item.Flags = append(item.Flags, fmt.Sprintf("【高風險】此為 %s，節點失效時將無法自動重啟或轉移。", wl.Kind))
		}
		// 如果發現 RWO 但副本數 > 1，增加警告
		if strings.Contains(wl.PVModes, "ReadWriteOnce") && wl.Replicas > 1 && wl.Kind != "StatefulSet" {
			item.Flags = append(item.Flags, "【警告】多副本工作負載使用 RWO PV，可能導致 Pod 漂移時因掛載競爭而啟動失敗")
		}
		// 檢查 Topology Spread Constraints
		if len(wl.PodSpec.TopologySpreadConstraints) > 0 {
			item.Flags = append(item.Flags, "【資訊】設定了 Topology Spread Constraints，實際可用性取決於拓撲分佈狀態")
		}
		// 靜態資源風險檢查
		if !wl.HasReq {
			item.Flags = append(item.Flags, "【高風險】未設定 Resource Requests (BestEffort QoS)，在資源緊繃時會優先被驅逐")
		}
		if !wl.HasProbe {
			item.Flags = append(item.Flags, "【注意】未設定 Liveness/Readiness Probes，無法保證 Pod 轉移後服務的健康狀態")
		}

		report = append(report, item)
	}

	return report
}

func IsNodeLogicallyEligible(podSpec corev1.PodSpec, node corev1.Node) bool {
	// 檢查 Taint 與 Toleration
	for _, taint := range node.Spec.Taints {
		if !HasMatchingToleration(podSpec.Tolerations, taint) {
			return false
		}
	}
	// 檢查 Node Selector
	if len(podSpec.NodeSelector) > 0 {
		for key, val := range podSpec.NodeSelector {
			if nodeVal, exists := node.Labels[key]; !exists || nodeVal != val {
				return false
			}
		}
	}
	// 檢查 Node Affinity (硬性限制)
	if podSpec.Affinity != nil && podSpec.Affinity.NodeAffinity != nil {
		req := podSpec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution
		if req != nil && len(req.NodeSelectorTerms) > 0 {
			if !MatchNodeSelectorTerms(node.Labels, req.NodeSelectorTerms) {
				return false
			}
		}
	}
	return true
}

func CheckInterWorkloadAntiAffinity(podSpec corev1.PodSpec, myLabels map[string]string) bool {
	if podSpec.Affinity == nil || podSpec.Affinity.PodAntiAffinity == nil {
		return false
	}
	myLabelSet := labels.Set(myLabels)
	for _, rule := range podSpec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		selector, err := metav1.LabelSelectorAsSelector(rule.LabelSelector)
		if err == nil {
			if !selector.Matches(myLabelSet) {
				return true
			}
		}
	}
	return false
}

func CheckSelfAntiAffinity(podSpec corev1.PodSpec, myLabels map[string]string) bool {
	if podSpec.Affinity == nil || podSpec.Affinity.PodAntiAffinity == nil {
		return false
	}
	myLabelSet := labels.Set(myLabels)
	for _, rule := range podSpec.Affinity.PodAntiAffinity.RequiredDuringSchedulingIgnoredDuringExecution {
		selector, err := metav1.LabelSelectorAsSelector(rule.LabelSelector)
		if err == nil {
			if selector.Matches(myLabelSet) {
				return true
			}
		}
	}
	return false
}

func CalculateAllowedFailures(eligibleNodeCount int, replicas int32, workloadType string, podSpec corev1.PodSpec, hasSelfAntiAffinity bool) int {
	if eligibleNodeCount == 0 {
		return 0
	}

	if replicas == 1 {
		return 0
	}

	switch workloadType {
	case "Deployment", "ReplicaSet", "DaemonSet":
		return max(0, eligibleNodeCount-1)

	case "StatefulSet":
		if HasLocalStorageAttached(podSpec) {
			return 0
		}
		return max(0, eligibleNodeCount-1)

	case "Pod", "Static Pod":
		// 獨立的 Pod 沒有控制器 (Controller) 幫忙做故障轉移 (Failover)。
		// 所在的節點一掛，Pod 就沒了，因此在架構上容忍節點失效的數量為 0。
		return 0
	}
	return 0
}

// ============== Helper Functions ==============

func HasMatchingToleration(tolerations []corev1.Toleration, taint corev1.Taint) bool {
	for _, tol := range tolerations {
		if tol.Key == taint.Key && (tol.Operator == corev1.TolerationOpExists || tol.Value == taint.Value) && (tol.Effect == "" || tol.Effect == taint.Effect) {
			return true
		}
	}
	return false
}

func MatchNodeSelectorTerms(nodeLabels map[string]string, terms []corev1.NodeSelectorTerm) bool {
	for _, term := range terms {
		match := true
		for _, expr := range term.MatchExpressions {
			val, exists := nodeLabels[expr.Key]
			switch expr.Operator {
			case corev1.NodeSelectorOpIn:
				if !exists || !contains(expr.Values, val) {
					match = false
				}
			case corev1.NodeSelectorOpNotIn:
				if exists && contains(expr.Values, val) {
					match = false
				}
			case corev1.NodeSelectorOpExists:
				if !exists {
					match = false
				}
			case corev1.NodeSelectorOpDoesNotExist:
				if exists {
					match = false
				}
			}
		}
		if match {
			return true
		}
	}
	return false
}

func HasLocalStorageAttached(podSpec corev1.PodSpec) bool {
	for _, vol := range podSpec.Volumes {
		// 檢查是否有 HostPath (直接綁定特定節點路徑)
		if vol.HostPath != nil {
			return true
		}
	}
	return false
}

func contains(slice []string, item string) bool {
	for _, s := range slice {
		if s == item {
			return true
		}
	}
	return false
}

// ============== 列印輸出邏輯 ==============

func PrintReport(reports []ReportItem) {
	fmt.Println("====================================== OCP 叢集可用性報告 ======================================")
	for _, r := range reports {
		fmt.Printf("[%s] %s (%s) | Replicas: %d\n", r.Namespace, r.WorkloadName, r.Type, r.Replicas)
		fmt.Printf("  - 可調度節點數: %d\n", r.EligibleNodeCount)
		fmt.Printf("  - PV 存取模式: %s\n", r.PVAccessModes)
		fmt.Printf("  - Priority Class: %s\n", r.PriorityClass)
		fmt.Printf("  - 資源請求: %v, Probes: %v\n", r.HasResourceRequests, r.HasProbes)
		fmt.Printf("  - PDB 狀態: %s\n", r.PDBStatus)
		fmt.Printf("  - 允許失效節點數: %d\n", r.AllowedNodeFailures)

		if len(r.Flags) > 0 {
			for _, flag := range r.Flags {
				fmt.Printf("  * %s\n", flag)
			}
		}
		fmt.Println("------------------------------------------------------------------------------------------------")
	}
}
