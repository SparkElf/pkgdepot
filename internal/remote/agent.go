package remote

import (
	"net"

	"golang.org/x/crypto/ssh/agent"
)

// newAgent 把 SSH_AUTH_SOCK 上的连接包装成 agent 客户端。
// 单独一层是为了让 ssh.go 的认证装配逻辑保持可读。
func agentClient(conn net.Conn) agent.ExtendedAgent {
	return agent.NewClient(conn)
}
