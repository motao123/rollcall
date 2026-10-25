# 安全策略

## 报告漏洞

发现问题请通过 GitHub Security Advisories 提交：
**[github.com/motao123/rollcall/security/advisories](https://github.com/motao123/rollcall/security/advisories)**

请勿在公开 Issue 中描述可被利用的细节。我们会在确认后发布修复版本，
并在致谢中列出报告者（除非你要求匿名）。

## 支持范围

- 最新 tag 与 main 分支
- 更早版本只在影响严重（如 RCE、鉴权绕过）时回合修复

## 设计边界（报告前值得一读）

Rollcall 的核心安全承诺是 **Agent 只读**：不 import `os/exec`、不监听端口、
服务端不下发任何指令。验证方式见 README「一、为什么做这个项目」。
因此以下情况属于设计边界而非漏洞：Agent 无法执行命令、无法反向连接、
面板无法向节点下发任务。

发行完整性：发布产物带 Ed25519 签名（`.sig`），升级脚本在替换前强制验签；
密钥体系与轮换流程见 `docs/operations.md`。
