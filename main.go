package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"
	"github.com/xuri/excelize/v2"
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
}

func main() {
	// 1. 初始化 Kubernetes Client
	config, err := getKubeConfig()
	if err != nil {
		fmt.Printf("取得 kubeconfig 失敗: %v\n", err)
		os.Exit(1)
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		fmt.Printf("建立 clientset 失敗: %v\n", err)
		os.Exit(1)
	}

	ctx := context.TODO()

	// 2. 抓取叢集內所有 Node
	fmt.Println("正在抓取 Nodes...")
	nodesObj, err := clientset.CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Printf("抓取 Nodes 失敗: %v\n", err)
		os.Exit(1)
	}

	// 3. 抓取所有 Namespace 的 Workloads (Deployments, StatefulSets, DaemonSets)
	fmt.Println("正在掃描所有 Namespace 的 Workloads...")
	var workloads []WorkloadSummary

	// 3-1. Deployments
	deps, err := clientset.AppsV1().Deployments("").List(ctx, metav1.ListOptions{})
	if err == nil {
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
			})
		}
	}

	// 3-2. StatefulSets
	sts, err := clientset.AppsV1().StatefulSets("").List(ctx, metav1.ListOptions{})
	if err == nil {
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
			})
		}
	}

	// 3-3. DaemonSets
	dsets, err := clientset.AppsV1().DaemonSets("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, ds := range dsets.Items {
			workloads = append(workloads, WorkloadSummary{
				Namespace: ds.Namespace,
				Name:      ds.Name,
				Kind:      "DaemonSet",
				Replicas:  0, // DaemonSet 的副本數隨節點變動，此處不適用固定數字
				Labels:    ds.Spec.Template.Labels,
				PodSpec:   ds.Spec.Template.Spec,
			})
		}
	}

        // 3-4. Standalone Pods (無 Controller 管理的獨立 Pod)
	fmt.Println("正在掃描獨立的 Bare Pods...")
	podsObj, err := clientset.CoreV1().Pods("").List(ctx, metav1.ListOptions{})
	if err == nil {
		for _, p := range podsObj.Items {
			// 檢查是否有 OwnerReferences。如果長度為 0，代表它是單獨存在的 Pod
			// 同時過濾掉已經執行完畢 (Succeeded) 或失敗 (Failed) 的 Pod
			if len(p.OwnerReferences) == 0 && p.Status.Phase != corev1.PodSucceeded && p.Status.Phase != corev1.PodFailed {
				workloads = append(workloads, WorkloadSummary{
					Namespace: p.Namespace,
					Name:      p.Name,
					Kind:      "Pod", // 標記為單一 Pod
					Replicas:  1,
					Labels:    p.Labels,
					PodSpec:   p.Spec,
				})
			}
		}
	}

	// 4. 產生報告
	fmt.Println("開始分析可用性與容錯機制...\n")
	reports := GenerateOcpAvailabilityReport(nodesObj.Items, workloads)

	// 5. 輸出報告 (終端機格式化輸出)
	PrintReport(reports)

	fileName := "ocp_availability_report.xlsx"
	err = ExportToExcel(reports, fileName)
	if err != nil {
		fmt.Printf("匯出 Excel 失敗: %v\n", err)
	} else {
		fmt.Printf("成功匯出報告至: %s\n", fileName)
	}
}


// ExportToExcel 實作匯出邏輯
func ExportToExcel(reports []ReportItem, filename string) error {
	f := excelize.NewFile()
	sheetName := "Availability Report"
	f.SetSheetName("Sheet1", sheetName)

	// 設定表頭
	headers := []string{"Namespace", "Workload Name", "Type", "Replicas", "Eligible Nodes Count", "Allowed Node Failures", "Flags / Warnings"}
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
		f.SetCellValue(sheetName, fmt.Sprintf("E%d", row), r.EligibleNodeCount)
		f.SetCellValue(sheetName, fmt.Sprintf("F%d", row), r.AllowedNodeFailures)
		f.SetCellValue(sheetName, fmt.Sprintf("G%d", row), strings.Join(r.Flags, " | "))
	}

	// 套用基本的樣式 (加粗表頭)
	style, _ := f.NewStyle(&excelize.Style{
		Font: &excelize.Font{Bold: true},
		Fill: excelize.Fill{Type: "pattern", Color: []string{"#E0E0E0"}, Pattern: 1},
	})
	f.SetCellStyle(sheetName, "A1", "G1", style)

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
			Flags:               []string{},
		}

		if hasSelfAntiAffinity {
			item.Flags = append(item.Flags, fmt.Sprintf("【注意】具備 Self Anti-Affinity，至少需 %d 台可用節點", wl.Replicas))
		}
		if hasInterWorkloadAntiAffinity {
			item.Flags = append(item.Flags, "【風險】設有 Inter-Workload Anti-Affinity，受其他服務位置影響")
		}
		if wl.Kind == "Pod" {
			item.Flags = append(item.Flags, "【高風險】此為無 Controller 管理的獨立 Pod (Bare Pod)，節點失效時將無法自動重啟或轉移。")
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
	switch workloadType {
	case "Deployment", "ReplicaSet":
		if hasSelfAntiAffinity {
			if eligibleNodeCount <= int(replicas) {
				return 0
			}
			return eligibleNodeCount - int(replicas)
		}
		return max(0, eligibleNodeCount-1)

	case "StatefulSet":
		if HasLocalStorageAttached(podSpec) {
			return 0
		}
		return max(0, eligibleNodeCount-1)

	case "DaemonSet":
		return max(0, eligibleNodeCount-1)

	case "Pod":
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
				if !exists || !contains(expr.Values, val) { match = false }
			case corev1.NodeSelectorOpNotIn:
				if exists && contains(expr.Values, val) { match = false }
			case corev1.NodeSelectorOpExists:
				if !exists { match = false }
			case corev1.NodeSelectorOpDoesNotExist:
				if exists { match = false }
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
		fmt.Printf("  - 允許失效節點數: %d\n", r.AllowedNodeFailures)
		
		if len(r.Flags) > 0 {
			for _, flag := range r.Flags {
				fmt.Printf("  * %s\n", flag)
			}
		}
		fmt.Println("------------------------------------------------------------------------------------------------")
	}
}
