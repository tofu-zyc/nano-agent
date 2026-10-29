# 发布流程

仓库为 [tofu-zyc/nano-agent](https://github.com/tofu-zyc/nano-agent)。流程以 [.github/workflows/ci.yml](../.github/workflows/ci.yml) 和 [release.yml](../.github/workflows/release.yml) 为准。

## 从开发分支发布

1. 在 dev/<版本> 完成实现与相关验证，检查变更、版本说明和敏感信息；不提交真实配置、密钥、data 或测试临时文件。
2. 提交并推送开发分支，等待该提交的 CI。Linux 跑测试和界面回归，Windows/macOS runner 跑相应原生测试；本地没设备不代表远程结果已经通过。
3. 用户同意发布后，将开发分支合入 main，推送并确认 main 上 CI 通过。不改写已推送历史，不强推。
4. 在 main 的已验证提交上创建 v<版本> 标签并推送。开发分支不打发布标签。
5. Release 工作流检查标签提交属于 main，再运行完整 CI，全部通过后构建并上传各平台包、VERSION、SHA256SUMS 和安装脚本。
6. 检查 Release、附件和校验和，使用临时安装目录验证安装、启动及必要的升级恢复路径。

标签触发真实发布与用户更新检查，不能用正式 v* 标签代替临时构建。CI 或发布失败时保留失败状态，修复原因后重试，不把上传成功当成功能验证。

## 升级与迁移

- 安装和手动升级入口见 [README](../README.md#安装)；Windows 自动更新仍禁用，重跑新包安装脚本。
- 正式安装保留用户配置、人格和技能；迁移与恢复规则见 [存储说明](STORAGE-v0.9.0.md)。
- 迁移整个网关使用 nano-gateway export/import。导出包包含解密所需密钥，按密码保管。
- 降级前停止新版并完整备份安装目录。旧程序不能读取新的 Hook journal 格式 2，不能只替换程序或混用新旧日志备份。

v0.9.0 本地验收覆盖 Linux + Windows；macOS 原生验收按用户要求暂缓。打包平台、实际 CI 结果与已验收范围分别说明。
