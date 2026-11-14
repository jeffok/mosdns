package ros_addrlist

import (
	"context"
	"os"
	"testing"
	"time"
)

// 真实 ROS API 端到端测试：需要环境变量
//   ROS_API_ADDR=192.168.88.254:8728 ROS_API_USER=mosdns ROS_API_PASS=xxx
// 只在显式提供凭据时运行。
func TestRealAPIWrite(t *testing.T) {
	addr := os.Getenv("ROS_API_ADDR")
	user := os.Getenv("ROS_API_USER")
	pass := os.Getenv("ROS_API_PASS")
	if addr == "" || user == "" || pass == "" {
		t.Skip("ROS_API_ADDR/USER/PASS not set")
	}

	a := &Args{
		Address:      addr,
		Username:     user,
		Password:     pass,
		ListName:     "gfw-proxy",
		Timeout:      "5m",
		Comment:      "mosdns-e2e-test",
		DialTimeout:  "5s",
		WriteTimeout: "5s",
		ReconnDelay:  "200ms",
	}
	rc, err := newROSClient(a)
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// 写入一条测试 IP（用文档保留段 203.0.113.99，不会被真路由影响，但要能被 add 接受——ROS 接受任意 IP）
	if err := rc.Add(ctx, "203.0.113.99"); err != nil {
		t.Fatalf("Add failed: %v", err)
	}
	t.Log("Add ok")

	// 清理
	cl, err := rc.get(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = cl.RunArgsContext(ctx, []string{
		"/ip/firewall/address-list/remove",
		"[find where address=203.0.113.99 and comment=mosdns-e2e-test]",
	})
	if err != nil {
		t.Logf("cleanup warn: %v", err)
	}
}
