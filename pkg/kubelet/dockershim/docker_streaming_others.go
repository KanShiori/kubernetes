//go:build !windows && !dockerless
// +build !windows,!dockerless

/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package dockershim

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"k8s.io/klog/v2"
)

// portForward 设定将 stream 输入转发到 pod 的 tcp4 port, 并将结果从 stream 返回
func (r *streamingRuntime) portForward(podSandboxID string, port int32,
	stream io.ReadWriteCloser) error {
	// 得到 pause 容器信息, 主要是其 pid
	container, err := r.client.InspectContainer(podSandboxID)
	if err != nil {
		return err
	}

	if !container.State.Running {
		return fmt.Errorf("container not running (%s)", container.ID)
	}

	// 拼接命令行 nsenter -t [container_pid] -n socat - TCP4:localhost:[port]
	// 通过 socat, 对应命令的 stdin 都会转发到 TCP4:localhost:[port]
	containerPid := container.State.Pid
	socatPath, lookupErr := exec.LookPath("socat")
	if lookupErr != nil {
		return fmt.Errorf("unable to do port forwarding: socat not found")
	}

	args := []string{"-t", fmt.Sprintf("%d", containerPid), "-n",
	 socatPath, "-", fmt.Sprintf("TCP4:localhost:%d", port)}

	nsenterPath, lookupErr := exec.LookPath("nsenter")
	if lookupErr != nil {
		return fmt.Errorf("unable to do port forwarding: nsenter not found")
	}

	commandString := fmt.Sprintf("%s %s", nsenterPath, strings.Join(args, " "))
	klog.V(4).InfoS("Executing port forwarding command", "command", commandString)

	// 创建命令, 设置结果返回给 [stream]
	// stderr 用于判断命令错误退出信息
	command := exec.Command(nsenterPath, args...)
	command.Stdout = stream

	stderr := new(bytes.Buffer)
	command.Stderr = stderr

	// 通过 StdinPipe() 与 io.Copy() 不断转发从 [stream] 的数据
	// If we use Stdin, command.Run() won't return until the goroutine that's copying
	// from stream finishes. Unfortunately, if you have a client like telnet connected
	// via port forwarding, as long as the user's telnet client is connected to the user's
	// local listener that port forwarding sets up, the telnet session never exits. This
	// means that even if socat has finished running, command.Run() won't ever return
	// (because the client still has the connection and stream open).
	//
	// The work around is to use StdinPipe(), as Wait() (called by Run()) closes the pipe
	// when the command (socat) exits.
	inPipe, err := command.StdinPipe()
	if err != nil {
		return fmt.Errorf("unable to do port forwarding: error creating stdin pipe: %v", err)
	}
	go func() {
		io.Copy(inPipe, stream)
		inPipe.Close()
	}()

	// 运行 command
	if err := command.Run(); err != nil {
		return fmt.Errorf("%v: %s", err, stderr.String())
	}

	return nil
}
