//go:build integration

package run_test

// 节点服务（多节点协作）在 -p 入口上的行为：**不承载**。
//
// 这条 spec 钉的是设计文档 §11 的入口表 —— -p 不自动承载节点，即使用户在配置里
// 打开了开关。一次性的管道运行没有"谁在承载"这回事，而隐式启用会让脚本里的一次
// 调用意外占住本机的节点身份，把正在跑的那个实例挤成"附着方"。
//
// 顺带它钉住了"零副作用"的另一半：开着也不建数据目录。关闭时零副作用由
// node/config_test.go 断言，两边合起来才是那句承诺。

import (
	"os"
	"path/filepath"

	"github.com/monsterxx03/tachi/itest/harness"
	"github.com/monsterxx03/tachi/itest/mockllm"
	"github.com/monsterxx03/tachi/itest/run"
	"github.com/onsi/ginkgo/v2"
	"github.com/onsi/gomega"
)

var _ = ginkgo.Describe("node service", func() {
	ginkgo.It("-p 不承载节点，即使 node.enabled: true", func() {
		mock := mockllm.NewServer()
		ginkgo.DeferCleanup(mock.Close)
		mock.Script(mockllm.Step{
			Reply: mockllm.Stream(
				mockllm.Text("你好"),
				mockllm.Finish("stop"),
				mockllm.Done(),
			),
		})
		home := harness.NewHome(ginkgo.GinkgoT(), mock, harness.WithNodeEnabled())
		workdir := harness.SeedWorkDir(ginkgo.GinkgoT(), nil)

		res := run.Binary(bin, workdir, "--home", home, "-p", "你好", "-o", "json-stream")

		gomega.Expect(res.Err).To(gomega.BeNil())
		gomega.Expect(res.ExitCode).To(gomega.Equal(0), "stdout=%s stderr=%s", res.Stdout, res.Stderr)

		_, err := os.Stat(filepath.Join(home, "a2a"))
		gomega.Expect(os.IsNotExist(err)).To(gomega.BeTrue(),
			"-p 运行不得创建节点数据目录")
	})
})
