// Package remote 是 pkgdepot 的 SSH 传输层：连接内网目标主机、执行 shell 脚本、
// 通过 SFTP 收发文件。
//
// 目标机只被假设有 sh + docker + tar（ADR-4），所以这里不做任何"在远端装 agent"
// 的尝试：所有复杂逻辑都以脚本文本发给远端 sh，文件走 SFTP。签名见
// docs/interfaces.md，属冻结集成面。
package remote
