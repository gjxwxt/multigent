# 端点交叉底稿（B2 机械部分）

> **STOP 提示**：本底稿只做机械交叉，不做任何保留/延后/废弃裁决。
> 130→45 的裁剪是产品级决策（v3 §7.2），由用户逐项拍板。

- 后端注册（internal/api/server.go，全部 mux）：**463**
- 前端字面量（web/src 全部 ts/tsx，含模板串静态前缀）：**316**
- frontend-called：**309**
- zero-call：**74**
- platform-internal：**74**
- websocket-surface：**6**
- 前端字面量未命中注册：0（见文末，多为 UI 路由或带完整查询串的拼接）

**WS 面说明**：`agent_channel_ws.go` 是出站 WebSocket 桥（refreshAgentIMBridges 拨向 Lark/Mattermost），不注册 HTTP 路由；入站实时面为 SSE（agent chat / preview live / assistant chat，已按 handler 归入 websocket-surface 档）。v3 §7.2 的 "agent_channel_ws + events" 与此对齐。

## zero-call（74）

| method | pattern | mux | handler | 命中字面量数 | 示例 |
|---|---|---|---|---|---|
| GET | `GET /api/v1/agent-sessions/{id}` | mux | handleAgentSession | 0 |  |
| DELETE | `DELETE /api/v1/agents/{id}/channels/{provider}` | mux | handleAgentWorkerChannelDelete | 0 |  |
| POST | `POST /api/v1/agents/{id}/channels/{provider}/bind-code` | mux | handleAgentWorkerChannelBindCode | 0 |  |
| GET | `GET /api/v1/agents/{id}/channels/{provider}/identities` | mux | handleAgentWorkerChannelIdentities | 0 |  |
| PUT | `PUT /api/v1/agents/{id}/channels/{provider}/security` | mux | handleAgentWorkerChannelSecurity | 0 |  |
| POST | `POST /api/v1/agents/{id}/channels/{provider}/setup/begin` | mux | handleAgentWorkerChannelSetupBegin | 0 |  |
| POST | `POST /api/v1/agents/{id}/channels/{provider}/setup/manual` | mux | handleAgentWorkerChannelSetupManual | 0 |  |
| POST | `POST /api/v1/agents/{id}/channels/{provider}/setup/poll` | mux | handleAgentWorkerChannelSetupPoll | 0 |  |
| GET | `GET /api/v1/agents/{id}/channels/{provider}/targets` | mux | handleAgentWorkerChannelTargets | 0 |  |
| DELETE | `DELETE /api/v1/agents/{id}/tool-bindings/{bindingId}` | mux | handleDeleteAgentWorkerToolBinding | 0 |  |
| PATCH | `PATCH /api/v1/attention/{id}` | mux | handlePatchAttentionSignal | 0 |  |
| GET | `GET /api/v1/connectors/providers/{provider}` | mux | handleConnectorProvider | 0 |  |
| GET | `GET /api/v1/daemon/status` | mux | handleDaemonStatus | 0 |  |
| GET | `GET /api/v1/files` | mux | handleListFiles | 0 |  |
| GET | `GET /api/v1/inbox` | mux | handleInbox | 0 |  |
| GET | `GET /api/v1/knowledge-base/artifacts` | mux | handleContextArtifacts | 0 |  |
| GET | `GET /api/v1/knowledge-base/collectors` | mux | handleContextCollectors | 0 |  |
| POST | `POST /api/v1/knowledge-base/import` | mux | handleContextImport | 0 |  |
| POST | `POST /api/v1/knowledge-base/import/file` | mux | handleContextImportFile | 0 |  |
| POST | `POST /api/v1/knowledge-base/import/manual` | mux | handleContextImportManual | 0 |  |
| GET | `GET /api/v1/knowledge-base/items` | mux | handleContextCenterItems | 0 |  |
| POST | `POST /api/v1/knowledge-base/items` | mux | handleContextCenterItems | 0 |  |
| POST | `POST /api/v1/knowledge-base/items/batch` | mux | handleContextCenterItemsBatch | 0 |  |
| GET | `GET /api/v1/knowledge-base/items/{id}` | mux | handleContextCenterItemGet | 0 |  |
| GET | `GET /api/v1/knowledge-base/subscriptions` | mux | handleContextCenterSubscriptions | 0 |  |
| POST | `POST /api/v1/knowledge-base/subscriptions` | mux | handleContextCenterSubscriptions | 0 |  |
| POST | `POST /api/v1/messages/mark-all-read` | mux | handlePostMarkAllMessagesRead | 0 |  |
| POST | `POST /api/v1/okrs/{id}/reviews` | mux | handleAddReviewNote | 0 |  |
| DELETE | `DELETE /api/v1/projects/{name}/agents/{agent}/channels/{provider}` | mux | handleAgentChannelDelete | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/channels/{provider}/bind-code` | mux | handleAgentChannelBindCode | 0 |  |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/channels/{provider}/identities` | mux | handleAgentChannelIdentities | 0 |  |
| PUT | `PUT /api/v1/projects/{name}/agents/{agent}/channels/{provider}/security` | mux | handleAgentChannelSecurity | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/channels/{provider}/setup/begin` | mux | handleAgentChannelSetupBegin | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/channels/{provider}/setup/manual` | mux | handleAgentChannelSetupManual | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/channels/{provider}/setup/poll` | mux | handleAgentChannelSetupPoll | 0 |  |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/channels/{provider}/targets` | mux | handleAgentChannelTargets | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/crons/{cronId}/pause` | mux | handlePostCronPause | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/crons/{cronId}/resume` | mux | handlePostCronResume | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/heartbeat/pause` | mux | handlePostHeartbeatPause | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/heartbeat/resume` | mux | handlePostHeartbeatResume | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/runtime-runs/exec` | mux | handleCreateRuntimeExecRun | 0 |  |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/runtime/connections` | mux | handleAgentRuntimeConnections | 0 |  |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/runtime/token` | mux | handleIssueAgentRuntimeToken | 0 |  |
| POST | `POST /api/v1/projects/{name}/brownfield/scan` | mux | handleBrownfieldScan | 0 |  |
| GET | `GET /api/v1/projects/{name}/deploy/preview-state` | mux | handleGetDeployPreviewState | 0 |  |
| POST | `POST /api/v1/projects/{name}/messages/mark-all-read` | mux | handlePostProjectMarkAllMessagesRead | 0 |  |
| POST | `POST /api/v1/projects/{name}/remote/verify` | mux | handleProjectRemoteVerify | 0 |  |
| POST | `POST /api/v1/projects/{name}/review-findings/import` | mux | handleReviewFindingsImport | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{id}/merge` | mux | handleMergeTaskMR | 0 |  |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/assets` | mux | handleListTaskAssets | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/assets` | mux | handleBindTaskAsset | 0 |  |
| DELETE | `DELETE /api/v1/projects/{name}/tasks/{taskId}/assets/{attachmentId}` | mux | handleUnbindTaskAsset | 0 |  |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/change-runs/{proposalId}` | mux | handleGetChangeRunProposal | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/design/chat` | mux | handleDesignChat | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/feedback` | mux | handlePostTaskPreviewFeedback | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/turns/{turnId}/preview/start` | mux | handlePostTaskPreviewTurnPreviewStart | 0 |  |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/turns/{turnId}/preview/status` | mux | handleGetTaskPreviewTurnPreviewStatus | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/turns/{turnId}/preview/stop` | mux | handlePostTaskPreviewTurnPreviewStop | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/remote-sync/retry` | mux | handlePostTaskRemoteSyncRetry | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/unblock` | mux | handleUnblockProjectTask | 0 |  |
| GET | `GET /api/v1/projects/{name}/workspace/health` | mux | handleGetProjectWorkspaceHealth | 0 |  |
| POST | `POST /api/v1/projects/{name}/workspace/rebuild` | mux | handleRebuildProjectWorkspace | 0 |  |
| GET | `GET /api/v1/prompts/agency` | mux | handleGetAgencyPrompt | 0 |  |
| PUT | `PUT /api/v1/prompts/agency` | mux | handlePutAgencyPrompt | 0 |  |
| POST | `POST /api/v1/providers/cc-switch/import` | mux | handleImportCCSwitchProviders | 0 |  |
| POST | `POST /api/v1/runtime-nodes/{id}/disable` | mux | handleDisableRuntimeNode | 0 |  |
| POST | `POST /api/v1/runtime-nodes/{id}/enable` | mux | handleEnableRuntimeNode | 0 |  |
| GET | `GET /api/v1/task-templates` | mux | handleListTaskTemplates | 0 |  |
| POST | `POST /api/v1/task-templates` | mux | handleCreateTaskTemplate | 0 |  |
| POST | `POST /api/v1/team-templates/{id}/apply` | mux | handleApplyTeamTemplate | 0 |  |
| POST | `POST /api/v1/workspaces/current/example` | mux | handleSeedCurrentExampleWorkspace | 0 |  |
| POST | `POST /api/v1/workspaces/{workspaceId}/workflow/triggers/{notificationId}/callback` | mux | handlePostWorkflowTriggerCallback | 0 |  |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/design/launch` | publicMux | handleDesignLaunch | 0 |  |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/feedback` | publicMux | handlePostTaskPreviewFeedback | 0 |  |

## frontend-called（309）

| method | pattern | mux | handler | 命中字面量数 | 示例 |
|---|---|---|---|---|---|
| GET | `GET /api/v1/agency` | mux | handleAgency | 1 | /api/v1/agency |
| GET | `GET /api/v1/agent-sessions` | mux | handleAgentSessions | 1 | /api/v1/agent-sessions?agentWorkerId=${encodeURIComponent(agentWorkerId)}&status=${encodeURIComponent(status)}&limit=20 |
| GET | `GET /api/v1/agents` | mux | handleAgentWorkers | 1 | /api/v1/agents |
| POST | `POST /api/v1/agents` | mux | handleCreateAgentWorker | 1 | /api/v1/agents |
| DELETE | `DELETE /api/v1/agents/{id}` | mux | handleDeleteAgentWorker | 5 | /api/v1/agents/${encodeURIComponent(agent.id)} (+4) |
| GET | `GET /api/v1/agents/{id}` | mux | handleAgentWorker | 5 | /api/v1/agents/${encodeURIComponent(agent.id)} (+4) |
| PATCH | `PATCH /api/v1/agents/{id}` | mux | handlePatchAgentWorker | 5 | /api/v1/agents/${encodeURIComponent(agent.id)} (+4) |
| GET | `GET /api/v1/agents/{id}/attention` | mux | handleAgentAttentionSignals | 1 | /api/v1/agents/${encodeURIComponent(agent.id)}/attention?limit=8 |
| GET | `GET /api/v1/agents/{id}/channels` | mux | handleAgentWorkerChannels | 1 | /api/v1/agents/${encodeURIComponent(agentWorkerId)}/channels |
| GET | `GET /api/v1/agents/{id}/context` | mux | handleGetAgentWorkerContext | 2 | /api/v1/agents/${encodeURIComponent(agent.id)}/context (+1) |
| GET | `GET /api/v1/agents/{id}/context-bindings` | mux | handleAgentWorkerContextBindings | 1 | /api/v1/agents/${encodeURIComponent(agentWorkerId)}/context-bindings |
| PUT | `PUT /api/v1/agents/{id}/sandbox` | mux | handlePutAgentWorkerSandbox | 1 | /api/v1/agents/${encodeURIComponent(agentWorkerId)}/sandbox |
| GET | `GET /api/v1/agents/{id}/tool-bindings` | mux | handleListAgentWorkerToolBindings | 1 | /api/v1/agents/${encodeURIComponent(agentWorkerId)}/tool-bindings |
| POST | `POST /api/v1/agents/{id}/tool-bindings` | mux | handleUpsertAgentWorkerToolBinding | 1 | /api/v1/agents/${encodeURIComponent(agentWorkerId)}/tool-bindings |
| PUT | `PUT /api/v1/agents/{id}/wakeup` | mux | handlePutAgentWorkerWakeup | 1 | /api/v1/agents/${encodeURIComponent(current.id)}/wakeup |
| POST | `POST /api/v1/assistant/chat` | mux | handleAssistantChat | 1 | /api/v1/assistant/chat |
| PUT | `PUT /api/v1/assistant/settings` | mux | handleAssistantSettings | 1 | /api/v1/assistant/settings |
| GET | `GET /api/v1/assistant/status` | mux | handleAssistantStatus | 1 | /api/v1/assistant/status |
| GET | `GET /api/v1/auth/bootstrap` | mux | handleAuthBootstrap | 1 | /api/v1/auth/bootstrap |
| GET | `GET /api/v1/auth/me` | mux | handleAuthMe | 1 | /api/v1/auth/me |
| PUT | `PUT /api/v1/auth/password` | mux | handleChangePassword | 1 | /api/v1/auth/password |
| GET | `GET /api/v1/auth/settings` | mux | handleAuthSettings | 1 | /api/v1/auth/settings |
| PUT | `PUT /api/v1/auth/settings` | mux | handlePutAuthSettings | 1 | /api/v1/auth/settings |
| GET | `GET /api/v1/billing/entitlements` | mux | handleBillingEntitlements | 1 | /api/v1/billing/entitlements |
| GET | `GET /api/v1/check-update` | mux | handleCheckUpdate | 1 | /api/v1/check-update |
| GET | `GET /api/v1/client-tokens` | mux | handleClientTokensList | 1 | /api/v1/client-tokens |
| POST | `POST /api/v1/client-tokens` | mux | handleClientTokensCreate | 1 | /api/v1/client-tokens |
| DELETE | `DELETE /api/v1/client-tokens/{id}` | mux | handleClientTokensDelete | 1 | /api/v1/client-tokens/${encodeURIComponent(token.id)} |
| GET | `GET /api/v1/connections` | mux | handleListConnections | 1 | /api/v1/connections |
| POST | `POST /api/v1/connections` | mux | handleCreateConnection | 1 | /api/v1/connections |
| POST | `POST /api/v1/connections/health-check` | mux | handleRunConnectionHealthChecks | 1 | /api/v1/connections/${encodeURIComponent(connection.id)} |
| DELETE | `DELETE /api/v1/connections/{id}` | mux | handleDeleteConnection | 1 | /api/v1/connections/${encodeURIComponent(connection.id)} |
| GET | `GET /api/v1/connections/{id}` | mux | handleGetConnection | 1 | /api/v1/connections/${encodeURIComponent(connection.id)} |
| PUT | `PUT /api/v1/connections/{id}` | mux | handleUpdateConnection | 1 | /api/v1/connections/${encodeURIComponent(connection.id)} |
| GET | `GET /api/v1/connections/{id}/grants` | mux | handleListConnectionGrants | 1 | /api/v1/connections/${encodeURIComponent(connectionId)}/grants |
| POST | `POST /api/v1/connections/{id}/grants` | mux | handleCreateConnectionGrant | 1 | /api/v1/connections/${encodeURIComponent(connectionId)}/grants |
| DELETE | `DELETE /api/v1/connections/{id}/grants/{grantId}` | mux | handleDeleteConnectionGrant | 1 | /api/v1/connections/${encodeURIComponent(connectionId)}/grants/${encodeURIComponent(grant.id)} |
| POST | `POST /api/v1/connections/{id}/set-default` | mux | handleSetDefaultConnection | 1 | /api/v1/connections/${encodeURIComponent(connection.id)}/set-default |
| POST | `POST /api/v1/connections/{id}/test` | mux | handleTestConnection | 1 | /api/v1/connections/${encodeURIComponent(connection.id)}/test |
| GET | `GET /api/v1/connectors/providers` | mux | handleConnectorProviders | 1 | /api/v1/connectors/providers |
| POST | `POST /api/v1/connectors/providers/{provider}/setup/begin` | mux | handleConnectorProviderSetupBegin | 1 | /api/v1/connectors/providers/${encodeURIComponent(provider.provider)}/setup/begin |
| POST | `POST /api/v1/connectors/providers/{provider}/setup/poll` | mux | handleConnectorProviderSetupPoll | 2 | /api/v1/connectors/providers/${encodeURIComponent(provider.provider)}/setup/poll (+1) |
| GET | `GET /api/v1/docs` | mux | handleDocsList | 1 | /api/v1/docs |
| POST | `POST /api/v1/docs` | mux | handleDocsAdd | 1 | /api/v1/docs |
| GET | `GET /api/v1/docs/lint` | mux | handleDocsLint | 5 | /api/v1/docs/${current.id}?content=true (+4) |
| GET | `GET /api/v1/docs/query` | mux | handleDocsQuery | 5 | /api/v1/docs/${current.id}?content=true (+4) |
| GET | `GET /api/v1/docs/tree` | mux | handleDocsTree | 6 | /api/v1/docs/${current.id}?content=true (+5) |
| DELETE | `DELETE /api/v1/docs/{id}` | mux | handleDocsDelete | 6 | /api/v1/docs/${current.id}?content=true (+5) |
| GET | `GET /api/v1/docs/{id}` | mux | handleDocsGet | 6 | /api/v1/docs/${current.id}?content=true (+5) |
| PUT | `PUT /api/v1/docs/{id}` | mux | handleDocsUpdate | 6 | /api/v1/docs/${current.id}?content=true (+5) |
| GET | `GET /api/v1/docs/{id}/download` | mux | handleDocsDownload | 1 | /api/v1/docs/${d.id}/download |
| GET | `GET /api/v1/docs/{id}/refs` | mux | handleDocsGetRefs | 1 | /api/v1/docs/${doc.id}/refs |
| POST | `POST /api/v1/docs/{id}/refs` | mux | handleDocsAddRef | 1 | /api/v1/docs/${doc.id}/refs |
| DELETE | `DELETE /api/v1/docs/{id}/refs/{refId}` | mux | handleDocsRemoveRef | 1 | /api/v1/docs/${doc.id}/refs/${encodeURIComponent(refId)} |
| GET | `GET /api/v1/envvars` | mux | handleListEnvVars | 1 | /api/v1/envvars |
| POST | `POST /api/v1/envvars` | mux | handleCreateEnvVar | 1 | /api/v1/envvars |
| DELETE | `DELETE /api/v1/envvars/{id}` | mux | handleDeleteEnvVar | 2 | /api/v1/envvars/${editId} (+1) |
| PUT | `PUT /api/v1/envvars/{id}` | mux | handleUpdateEnvVar | 2 | /api/v1/envvars/${editId} (+1) |
| GET | `GET /api/v1/files/content/{path...}` | mux | handleFileContent | 1 | /api/v1/files/content/${encoded} |
| POST | `POST /api/v1/files/mkdir` | mux | handleMkdir | 2 | /api/v1/files/${encoded} (+1) |
| POST | `POST /api/v1/files/move` | mux | handleMoveFile | 2 | /api/v1/files/${encoded} (+1) |
| POST | `POST /api/v1/files/upload` | mux | handleUploadFile | 1 | /api/v1/files/${encoded} |
| DELETE | `DELETE /api/v1/files/{path...}` | mux | handleDeleteFile | 4 | /api/v1/files/${encoded} (+3) |
| GET | `GET /api/v1/integrations/gitlab/namespaces` | mux | handleGitLabNamespaces | 1 | /api/v1/integrations/gitlab/namespaces |
| POST | `POST /api/v1/integrations/gitlab/projects` | mux | handleGitLabCreateProject | 1 | /api/v1/integrations/gitlab/projects |
| GET | `GET /api/v1/integrations/gitlab/status` | mux | handleGitLabStatus | 1 | /api/v1/integrations/gitlab/status |
| GET | `GET /api/v1/invitations` | mux | handleListInvitations | 1 | /api/v1/invitations |
| POST | `POST /api/v1/invitations` | mux | handleCreateInvitation | 1 | /api/v1/invitations |
| POST | `POST /api/v1/invitations/{token}/revoke` | mux | handleRevokeInvitation | 1 | /api/v1/invitations/${encodeURIComponent(token)}/revoke |
| POST | `POST /api/v1/knowledge-base/bindings` | mux | handleContextCreateBinding | 1 | /api/v1/knowledge-base/bindings |
| DELETE | `DELETE /api/v1/knowledge-base/bindings/{id}` | mux | handleContextDeleteBinding | 1 | /api/v1/knowledge-base/bindings/${encodeURIComponent(id)} |
| GET | `GET /api/v1/knowledge-base/sources` | mux | handleContextCenterSources | 1 | /api/v1/knowledge-base/sources?limit=200 |
| POST | `POST /api/v1/knowledge-base/sources` | mux | handleContextCenterSources | 1 | /api/v1/knowledge-base/sources?limit=200 |
| POST | `POST /api/v1/messages` | mux | handlePostMessage | 1 | /api/v1/messages |
| POST | `POST /api/v1/messages/archive` | mux | handlePostArchiveMessage | 1 | /api/v1/messages/archive |
| POST | `POST /api/v1/messages/delete` | mux | handlePostDeleteMessage | 1 | /api/v1/messages/delete |
| POST | `POST /api/v1/messages/mark-read` | mux | handlePostMarkMessageRead | 1 | /api/v1/messages/mark-read |
| GET | `GET /api/v1/model-catalog` | mux | handleModelCatalog | 1 | /api/v1/model-catalog |
| POST | `POST /api/v1/oauth/authorizations` | mux | handleStartOAuthAuthorization | 1 | /api/v1/oauth/authorizations |
| GET | `GET /api/v1/oauth/client-configs` | mux | handleListOAuthClientConfigs | 1 | /api/v1/oauth/client-configs |
| DELETE | `DELETE /api/v1/oauth/client-configs/{provider}` | mux | handleDeleteOAuthClientConfig | 1 | /api/v1/oauth/client-configs/${encodeURIComponent(config.provider)} |
| PUT | `PUT /api/v1/oauth/client-configs/{provider}` | mux | handleUpsertOAuthClientConfig | 1 | /api/v1/oauth/client-configs/${encodeURIComponent(config.provider)} |
| GET | `GET /api/v1/okrs` | mux | handleListOKRs | 2 | /api/v1/okrs (+1) |
| POST | `POST /api/v1/okrs` | mux | handleCreateOKR | 2 | /api/v1/okrs (+1) |
| DELETE | `DELETE /api/v1/okrs/{id}` | mux | handleDeleteOKR | 2 | /api/v1/okrs/${id} (+1) |
| GET | `GET /api/v1/okrs/{id}` | mux | handleGetOKR | 2 | /api/v1/okrs/${id} (+1) |
| PUT | `PUT /api/v1/okrs/{id}` | mux | handleUpdateOKR | 2 | /api/v1/okrs/${id} (+1) |
| POST | `POST /api/v1/okrs/{id}/key-results` | mux | handleAddKR | 1 | /api/v1/okrs/${okrId}/key-results |
| DELETE | `DELETE /api/v1/okrs/{id}/key-results/{krId}` | mux | handleDeleteKR | 1 | /api/v1/okrs/${okrId}/key-results/${krId} |
| PUT | `PUT /api/v1/okrs/{id}/key-results/{krId}` | mux | handleUpdateKR | 1 | /api/v1/okrs/${okrId}/key-results/${krId} |
| GET | `GET /api/v1/playbook-installs` | mux | handleListPlaybookInstalls | 1 | /api/v1/playbook-installs |
| GET | `GET /api/v1/playbook-templates` | mux | handleListPlaybookTemplates | 1 | /api/v1/playbook-templates?locale=${encodeURIComponent(locale)} |
| GET | `GET /api/v1/playbook-templates/{playbookId}` | mux | handleGetPlaybookTemplate | 2 | /api/v1/playbook-templates/${encodeURIComponent(params.playbookId)}?locale=${encodeURIComponent(locale)} (+1) |
| POST | `POST /api/v1/playbook-templates/{playbookId}/install` | mux | handleInstallPlaybookTemplate | 1 | /api/v1/playbook-templates/${encodeURIComponent(playbook.id)}/install |
| GET | `GET /api/v1/projects` | mux | handleProjects | 1 | /api/v1/projects |
| POST | `POST /api/v1/projects` | mux | handleCreateProject | 1 | /api/v1/projects |
| GET | `GET /api/v1/projects/orphans` | mux | handleProjectOrphans | 2 | /api/v1/projects/${encodeURIComponent(projectId)} (+1) |
| DELETE | `DELETE /api/v1/projects/orphans/{name}` | mux | handleProjectOrphansDelete | 32 | /api/v1/projects/${encodeURIComponent(agentProject)}/agents (+31) |
| GET | `GET /api/v1/projects/runtime-inventory` | mux | handleProjectRuntimeInventory | 2 | /api/v1/projects/${encodeURIComponent(projectId)} (+1) |
| DELETE | `DELETE /api/v1/projects/{name}` | mux | handleDeleteProject | 2 | /api/v1/projects/${encodeURIComponent(projectId)} (+1) |
| GET | `GET /api/v1/projects/{name}` | mux | handleProject | 2 | /api/v1/projects/${encodeURIComponent(projectId)} (+1) |
| PUT | `PUT /api/v1/projects/{name}` | mux | handlePutProject | 2 | /api/v1/projects/${encodeURIComponent(projectId)} (+1) |
| GET | `GET /api/v1/projects/{name}/agents` | mux | handleProjectAgents | 6 | /api/v1/projects/${encodeURIComponent(agentProject)}/agents (+5) |
| DELETE | `DELETE /api/v1/projects/{name}/agents/{agent}` | mux | handleFireAgent | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)} (+1) |
| PATCH | `PATCH /api/v1/projects/{name}/agents/{agent}` | mux | handlePatchAgent | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)} (+1) |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/channels` | mux | handleAgentChannels | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/channels |
| DELETE | `DELETE /api/v1/projects/{name}/agents/{agent}/chat` | mux | handleAgentChatStop | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(agentName)}/chat (+1) |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/chat` | mux | handleAgentChat | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(agentName)}/chat (+1) |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/chat/history` | mux | handleAgentChatHistory | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agent)}/chat/history |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/chat/sessions` | mux | handleAgentChatSessions | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agent)}/chat/sessions |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/context` | mux | handleGetAgentContext | 3 | /api/v1/projects/${encodeURIComponent(project.name)}/agents/${encodeURIComponent(agent.name)}/context (+2) |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/context-bindings` | mux | handleAgentContextBindings | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/context-bindings |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/crons` | mux | handlePostCron | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/crons (+1) |
| DELETE | `DELETE /api/v1/projects/{name}/agents/{agent}/crons/{cronId}` | mux | handleDeleteCron | 3 | /api/v1/projects/${encodeURIComponent(cron.project)}/agents/${encodeURIComponent(agentName)}/crons/${encodeURIComponent(cron.id)} (+2) |
| PUT | `PUT /api/v1/projects/{name}/agents/{agent}/crons/{cronId}` | mux | handlePutCron | 3 | /api/v1/projects/${encodeURIComponent(cron.project)}/agents/${encodeURIComponent(agentName)}/crons/${encodeURIComponent(cron.id)} (+2) |
| DELETE | `DELETE /api/v1/projects/{name}/agents/{agent}/env` | mux | handleDeleteAgentEnv | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/env (+1) |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/env` | mux | handleGetAgentEnv | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/env (+1) |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/env` | mux | handleSetAgentEnv | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/env (+1) |
| PUT | `PUT /api/v1/projects/{name}/agents/{agent}/env` | mux | handlePutAgentEnv | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/env (+1) |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/heartbeat` | mux | handleGetHeartbeat | 3 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/heartbeat (+2) |
| PATCH | `PATCH /api/v1/projects/{name}/agents/{agent}/heartbeat` | mux | handlePatchHeartbeat | 3 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/heartbeat (+2) |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/interactions/active` | mux | handleAgentInteractionStatus | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/interactions/active |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/runtime/readiness` | mux | handleGetAgentRuntimeReadiness | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agent)}/runtime/readiness |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/runtime/slot` | mux | handleGetAgentRuntimeSlot | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/runtime/slot |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/runtime/slot/release` | mux | handleReleaseAgentRuntimeSlot | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/runtime/slot/release |
| PUT | `PUT /api/v1/projects/{name}/agents/{agent}/sandbox` | mux | handlePutAgentSandbox | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/sandbox |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/set-model` | mux | handleSetModel | 1 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/set-model |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/tool-bindings` | mux | handleListAgentToolBindings | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/tool-bindings (+1) |
| POST | `POST /api/v1/projects/{name}/agents/{agent}/tool-bindings` | mux | handleUpsertAgentToolBinding | 2 | /api/v1/projects/${encodeURIComponent(project)}/agents/${encodeURIComponent(agentName)}/tool-bindings (+1) |
| DELETE | `DELETE /api/v1/projects/{name}/agents/{agent}/tool-bindings/{bindingId}` | mux | handleDeleteAgentToolBinding | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(binding.agentId \|\| '')}/tool-bindings/${encodeURIComponent(binding.id)} (+1) |
| PUT | `PUT /api/v1/projects/{name}/agents/{agent}/wakeup` | mux | handlePutAgentWakeup | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(agentName)}/wakeup |
| GET | `GET /api/v1/projects/{name}/assets` | mux | handleListProjectAssets | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/assets (+1) |
| POST | `POST /api/v1/projects/{name}/assets` | mux | handleProjectAssetUpload | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/assets (+1) |
| DELETE | `DELETE /api/v1/projects/{name}/assets/{fileId}` | mux | handleProjectAssetArchive | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/assets/${encodeURIComponent(row.id)} |
| GET | `GET /api/v1/projects/{name}/assets/{fileId}` | mux | handleProjectAssetDetail | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/assets/${encodeURIComponent(row.id)} |
| PATCH | `PATCH /api/v1/projects/{name}/assets/{fileId}` | mux | handleProjectAssetRename | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/assets/${encodeURIComponent(row.id)} |
| GET | `GET /api/v1/projects/{name}/assets/{fileId}/download` | mux | handleProjectAssetDownload | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/assets/${encodeURIComponent(row.id)}/download |
| GET | `GET /api/v1/projects/{name}/branches` | mux | handleListProjectBranches | 1 | /api/v1/projects/${encodeURIComponent(selectedProject)}/branches |
| POST | `POST /api/v1/projects/{name}/branches/refresh` | mux | handlePostProjectBranchesRefresh | 1 | /api/v1/projects/${encodeURIComponent(selectedProject)}/branches/refresh |
| GET | `GET /api/v1/projects/{name}/channels` | mux | handleListProjectChannels | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/channels |
| POST | `POST /api/v1/projects/{name}/channels/provision` | mux | handleProvisionProjectChannel | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/channels/provision |
| POST | `POST /api/v1/projects/{name}/delivery/preplan` | mux | handleDeliveryPreplan | 1 | /api/v1/projects/${encodeURIComponent(selectedProject)}/delivery/preplan |
| GET | `GET /api/v1/projects/{name}/deploy` | mux | handleGetDeployAggregate | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy |
| POST | `POST /api/v1/projects/{name}/deploy/ai/chat` | mux | handleDeployAIChat | 1 | /api/v1/projects/${encodeURIComponent(project)}/deploy/ai/chat |
| GET | `GET /api/v1/projects/{name}/deploy/ai/context` | mux | handleDeployAIContext | 1 | /api/v1/projects/${encodeURIComponent(project)}/deploy/ai/context |
| POST | `POST /api/v1/projects/{name}/deploy/ai/tasks` | mux | handleDeployAITasks | 1 | /api/v1/projects/${encodeURIComponent(project)}/deploy/ai/tasks |
| GET | `GET /api/v1/projects/{name}/deploy/branches` | mux | handleGetDeployBranches | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/branches |
| GET | `GET /api/v1/projects/{name}/deploy/requests` | mux | handleListDeployRequests | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests |
| POST | `POST /api/v1/projects/{name}/deploy/requests` | mux | handleCreateDeployRequest | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests |
| POST | `POST /api/v1/projects/{name}/deploy/requests/{id}/approve` | mux | handleApproveDeployRequest | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(id)}/${action} |
| POST | `POST /api/v1/projects/{name}/deploy/requests/{id}/cancel` | mux | handleCancelDeployRequest | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(id)}/${action} |
| POST | `POST /api/v1/projects/{name}/deploy/requests/{id}/reject` | mux | handleRejectDeployRequest | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(id)}/${action} |
| POST | `POST /api/v1/projects/{name}/deploy/requests/{id}/trigger` | mux | handleTriggerDeployRequest | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(created.id)}/trigger (+1) |
| GET | `GET /api/v1/projects/{name}/deploy/requests/{id}/verification` | mux | handleGetDeployVerification | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(id)}/${action} (+1) |
| POST | `POST /api/v1/projects/{name}/deploy/requests/{id}/verification/generate` | mux | handleGenerateDeployVerification | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(requestId)}/verification/generate |
| POST | `POST /api/v1/projects/{name}/deploy/requests/{id}/verification/run` | mux | handleRunDeployVerification | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/deploy/requests/${encodeURIComponent(requestId)}/verification/run |
| POST | `POST /api/v1/projects/{name}/hire` | mux | handleHireAgent | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/hire |
| GET | `GET /api/v1/projects/{name}/initialization` | mux | handleGetProjectInitialization | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/initialization |
| POST | `POST /api/v1/projects/{name}/initialize-template` | mux | handleInitializeProjectTemplate | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/initialize-template |
| GET | `GET /api/v1/projects/{name}/memberships` | mux | handleProjectMemberships | 3 | /api/v1/projects/${encodeURIComponent(p.name)}/memberships (+2) |
| POST | `POST /api/v1/projects/{name}/memberships` | mux | handleCreateProjectMembership | 3 | /api/v1/projects/${encodeURIComponent(p.name)}/memberships (+2) |
| DELETE | `DELETE /api/v1/projects/{name}/memberships/{membershipId}` | mux | handleDeleteProjectMembership | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/memberships/${encodeURIComponent(pendingDelete.id)} |
| GET | `GET /api/v1/projects/{name}/messages` | mux | handleProjectMessages | 1 | /api/v1/projects/${encodeURIComponent(project)}/messages?mailbox=${encodeURIComponent(mailbox)}&archived=all |
| GET | `GET /api/v1/projects/{name}/milestones` | mux | handleListMilestones | 1 | /api/v1/projects/${encodeURIComponent(project)}/milestones |
| POST | `POST /api/v1/projects/{name}/milestones` | mux | handleCreateMilestone | 1 | /api/v1/projects/${encodeURIComponent(project)}/milestones |
| DELETE | `DELETE /api/v1/projects/{name}/milestones/{msId}` | mux | handleDeleteMilestone | 2 | /api/v1/projects/${encodeURIComponent(project)}/milestones/${id} (+1) |
| GET | `GET /api/v1/projects/{name}/milestones/{msId}` | mux | handleGetMilestone | 2 | /api/v1/projects/${encodeURIComponent(project)}/milestones/${id} (+1) |
| PUT | `PUT /api/v1/projects/{name}/milestones/{msId}` | mux | handleUpdateMilestone | 2 | /api/v1/projects/${encodeURIComponent(project)}/milestones/${id} (+1) |
| GET | `GET /api/v1/projects/{name}/prompt` | mux | handleGetProjectPrompt | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/prompt |
| PUT | `PUT /api/v1/projects/{name}/prompt` | mux | handlePutProjectPrompt | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/prompt |
| POST | `POST /api/v1/projects/{name}/remote/verify-init` | mux | handleProjectInitRemoteVerify | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/remote/verify-init |
| GET | `GET /api/v1/projects/{name}/schedule` | mux | handleGetProjectSchedule | 3 | /api/v1/projects/${encodeURIComponent(item)}/schedule (+2) |
| POST | `POST /api/v1/projects/{name}/sync` | mux | handlePostProjectSync | 1 | /api/v1/projects/${encodeURIComponent(target.project)}/sync |
| GET | `GET /api/v1/projects/{name}/task-templates` | mux | handleListProjectTaskTemplates | 2 | /api/v1/projects/${encodeURIComponent(project)}/task-templates (+1) |
| POST | `POST /api/v1/projects/{name}/task-templates` | mux | handleCreateProjectTaskTemplate | 2 | /api/v1/projects/${encodeURIComponent(project)}/task-templates (+1) |
| GET | `GET /api/v1/projects/{name}/tasks` | mux | handleProjectTasks | 4 | /api/v1/projects/${encodeURIComponent(project)}/tasks?agent=${encodeURIComponent(member)} (+3) |
| POST | `POST /api/v1/projects/{name}/tasks` | mux | handlePostProjectTask | 4 | /api/v1/projects/${encodeURIComponent(project)}/tasks?agent=${encodeURIComponent(member)} (+3) |
| POST | `POST /api/v1/projects/{name}/tasks/from-template` | mux | handlePostProjectTaskFromTemplate | 1 | /api/v1/projects/${encodeURIComponent(selectedProject)}/tasks/from-template |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/change-runs` | mux | handleListChangeRunProposals | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/change-runs` | mux | handleCreateChangeRunProposal | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/change-runs/{proposalId}/apply` | mux | handleApplyChangeRunProposal | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs/${encodeURIComponent(proposal.id)}/${action} |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/change-runs/{proposalId}/reject` | mux | handleRejectChangeRunProposal | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs/${encodeURIComponent(proposal.id)}/${action} |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/change-runs/{proposalId}/rollback` | mux | handleRollbackChangeRunProposal | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/change-runs/${encodeURIComponent(proposal.id)}/${action} |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/design/audit-delta` | mux | handleDesignAuditDelta | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/audit-delta |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/design/projects` | mux | handleDesignListProjects | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/projects |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/design/rework` | mux | handleDesignRework | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design{VARSEP}/rework |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/design/snapshot/{path...}` | mux | handleGetDesignSnapshotFile | 1 | /api/v1/projects/${encodeURIComponent(project!)}/tasks/${encodeURIComponent(taskID!)}/design/snapshot/ |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/design/start` | mux | handleDesignStart | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design{VARSEP}/start |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/design/status` | mux | handleDesignStatus | 4 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/design/status (+3) |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/fixture-sandbox` | mux | handleGetTaskFixtureSandbox | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/fixture-sandbox |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/fixture-sandbox/reset` | mux | handleResetTaskFixtureSandbox | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/fixture-sandbox/reset |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/fixture-sandbox/scenario` | mux | handleSwitchTaskFixtureSandboxScenario | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/fixture-sandbox/scenario |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview` | mux | handleGetTaskPreview | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(taskId)}/preview (+1) |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/profiles` | mux | handleGetTaskPreviewProfiles | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/profiles |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/start` | mux | handlePostTaskPreviewStart | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(taskId)}/preview/start (+1) |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/stop` | mux | handlePostTaskPreviewStop | 2 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/stop (+1) |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/turns` | mux | handleGetTaskPreviewTurns | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/turns |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/turns/{turnId}/diff` | mux | handleGetTaskPreviewTurnDiff | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/turns/${encodeURIComponent(turn.turnId)}/diff |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/turns/{turnId}/rollback` | mux | handlePostTaskPreviewTurnRollback | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/turns/${encodeURIComponent(turnId)}/rollback |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/resources` | mux | handleGetTaskResources | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/resources |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/resources/worktree/cleanup` | mux | handlePostTaskWorktreeCleanup | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/resources/worktree/cleanup |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/start` | mux | handleStartProjectTask | 3 | /api/v1/projects/${encodeURIComponent(displayTask.project)}/tasks/${encodeURIComponent(displayTask.id)}/start (+2) |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/workflow` | mux | handleGetTaskWorkflow | 3 | /api/v1/projects/${encodeURIComponent(projectId)}/tasks/${encodeURIComponent(initializationTaskId)}/workflow (+2) |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/workflow/branch/{branchId}/defer` | mux | handlePostTaskBranchDefer | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/workflow/branch/${encodeURIComponent(row.branchId)}/${action} |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/workflow/branch/{branchId}/retry` | mux | handlePostTaskBranchRetry | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/workflow/branch/${encodeURIComponent(row.branchId)}/${action} |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/workflow/branch/{branchId}/undefer` | mux | handlePostTaskBranchUndefer | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/workflow/branch/${encodeURIComponent(row.branchId)}/${action} |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/workflow/delivery-plan` | mux | handleGetTaskDeliveryPlan | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskId)}/workflow/delivery-plan |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/workflow/review` | mux | handlePostTaskWorkflowReview | 2 | /api/v1/projects/${encodeURIComponent(displayTask.project)}/tasks/${encodeURIComponent(displayTask.id)}/workflow/review (+1) |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/workflow/review/draft` | mux | handleGetTaskWorkflowReviewDraft | 1 | /api/v1/projects/${encodeURIComponent(project)}/tasks/${encodeURIComponent(taskID)}/workflow/review/draft |
| GET | `GET /api/v1/projects/{name}/tool-bindings` | mux | handleListProjectToolBindings | 1 | /api/v1/projects/${encodeURIComponent(projectId)}/tool-bindings |
| POST | `POST /api/v1/projects/{name}/tool-bindings/install` | mux | handleInstallProjectToolBindings | 2 | /api/v1/projects/${encodeURIComponent(project)}/tool-bindings/install (+1) |
| GET | `GET /api/v1/prompts/roles` | mux | handleGetRolePrompt | 1 | /api/v1/prompts/roles?team=${encodeURIComponent(teamPath)}&role=${encodeURIComponent(roleKey)} |
| PUT | `PUT /api/v1/prompts/roles` | mux | handlePutRolePrompt | 1 | /api/v1/prompts/roles?team=${encodeURIComponent(teamPath)}&role=${encodeURIComponent(roleKey)} |
| GET | `GET /api/v1/prompts/teams/{teamPath...}` | mux | handleGetTeamPrompt | 1 | /api/v1/prompts/teams/${apiTeamPath(teamId)} |
| PUT | `PUT /api/v1/prompts/teams/{teamPath...}` | mux | handlePutTeamPrompt | 1 | /api/v1/prompts/teams/${apiTeamPath(teamId)} |
| GET | `GET /api/v1/providers` | mux | handleListProviders | 3 | /api/v1/providers (+2) |
| POST | `POST /api/v1/providers` | mux | handleAddProvider | 3 | /api/v1/providers (+2) |
| POST | `POST /api/v1/providers/auth/codex/device/begin` | mux | handleCodexDeviceAuthBegin | 1 | /api/v1/providers/auth/codex/device/begin |
| POST | `POST /api/v1/providers/auth/codex/device/poll` | mux | handleCodexDeviceAuthPoll | 1 | /api/v1/providers/auth/codex/device/poll |
| DELETE | `DELETE /api/v1/providers/auth/sessions/{sessionId}` | mux | handleModelAuthSessionCancel | 2 | /api/v1/providers/auth/sessions/${encodeURIComponent(previousSessionID)} (+1) |
| POST | `POST /api/v1/providers/auth/{cli}/browser/begin` | mux | handleCLIBrowserAuthBegin | 1 | /api/v1/providers/auth/${encodeURIComponent(cli)}/browser/begin |
| POST | `POST /api/v1/providers/auth/{cli}/browser/code` | mux | handleCLIBrowserAuthCode | 1 | /api/v1/providers/auth/${encodeURIComponent(deviceAuth.cli)}/browser/code |
| POST | `POST /api/v1/providers/auth/{cli}/browser/poll` | mux | handleCLIBrowserAuthPoll | 1 | /api/v1/providers/auth/${encodeURIComponent(cli)}/browser/poll |
| GET | `GET /api/v1/providers/cc-switch` | mux | handleListCCSwitchProviders | 2 | /api/v1/providers/${editing.id} (+1) |
| GET | `GET /api/v1/providers/local-import` | mux | handleListLocalModelProviders | 3 | /api/v1/providers/${editing.id} (+2) |
| POST | `POST /api/v1/providers/local-import/import` | mux | handleImportLocalModelProviders | 1 | /api/v1/providers/local-import/import |
| DELETE | `DELETE /api/v1/providers/{id}` | mux | handleDeleteProvider | 3 | /api/v1/providers/${editing.id} (+2) |
| PUT | `PUT /api/v1/providers/{id}` | mux | handleUpdateProvider | 3 | /api/v1/providers/${editing.id} (+2) |
| GET | `GET /api/v1/rbac/model` | mux | handleRBACModel | 1 | /api/v1/rbac/model |
| POST | `POST /api/v1/roles/create` | mux | handleCreateRole | 1 | /api/v1/roles/create |
| POST | `POST /api/v1/roles/skills` | mux | handlePostRoleSkillBind | 1 | /api/v1/roles/skills |
| POST | `POST /api/v1/run` | mux | handleRunAgent | 1 | /api/v1/run |
| GET | `GET /api/v1/runtime-nodes` | mux | handleRuntimeNodes | 1 | /api/v1/runtime-nodes |
| POST | `POST /api/v1/runtime-nodes/join-token` | mux | handleCreateRuntimeNodeJoinToken | 2 | /api/v1/runtime-nodes/${encodeURIComponent(node.id)} (+1) |
| DELETE | `DELETE /api/v1/runtime-nodes/{id}` | mux | handleDeleteRuntimeNode | 2 | /api/v1/runtime-nodes/${encodeURIComponent(node.id)} (+1) |
| GET | `GET /api/v1/runtime-nodes/{id}` | mux | handleRuntimeNode | 2 | /api/v1/runtime-nodes/${encodeURIComponent(node.id)} (+1) |
| GET | `GET /api/v1/sandbox/capabilities` | mux | handleSandboxCapabilities | 1 | /api/v1/sandbox/capabilities |
| POST | `POST /api/v1/scheduler/abort` | mux | handleSchedulerAbort | 1 | /api/v1/scheduler/abort |
| GET | `GET /api/v1/scheduler/status` | mux | handleSchedulerStatus | 1 | /api/v1/scheduler/status |
| POST | `POST /api/v1/scheduler/wakeup` | mux | handleSchedulerWakeup | 1 | /api/v1/scheduler/wakeup |
| POST | `POST /api/v1/session/reset` | mux | handleSessionReset | 1 | /api/v1/session/reset |
| GET | `GET /api/v1/skill-registry` | mux | handleListSkillRegistry | 1 | /api/v1/skill-registry |
| GET | `GET /api/v1/skills` | mux | handleListSkills | 1 | /api/v1/skills |
| POST | `POST /api/v1/skills` | mux | handleCreateSkill | 1 | /api/v1/skills |
| POST | `POST /api/v1/skills/install` | mux | handleInstallSkill | 2 | /api/v1/skills/${encodeURIComponent(skill.name)} (+1) |
| GET | `GET /api/v1/skills/local` | mux | handleListLocalSkills | 2 | /api/v1/skills/${encodeURIComponent(skill.name)} (+1) |
| POST | `POST /api/v1/skills/local/import` | mux | handleImportLocalSkills | 1 | /api/v1/skills/local/import |
| GET | `GET /api/v1/skills/{name}` | mux | handleGetSkillDetail | 3 | /api/v1/skills/${encodeURIComponent(skill.name)} (+2) |
| PATCH | `PATCH /api/v1/skills/{name}` | mux | handlePatchSkillMetadata | 3 | /api/v1/skills/${encodeURIComponent(skill.name)} (+2) |
| PUT | `PUT /api/v1/skills/{name}` | mux | handlePutSkillPrompt | 3 | /api/v1/skills/${encodeURIComponent(skill.name)} (+2) |
| GET | `GET /api/v1/skills/{name}/files` | mux | handleGetSkillFiles | 1 | /api/v1/skills/${encodeURIComponent(skill.name)}/files |
| GET | `GET /api/v1/stats` | mux | handleStats | 1 | /api/v1/stats |
| DELETE | `DELETE /api/v1/task-templates/{templateId}` | mux | handleDeleteTaskTemplate | 2 | /api/v1/task-templates/${encodeURIComponent(editingTemplate.id)} (+1) |
| GET | `GET /api/v1/task-templates/{templateId}` | mux | handleGetTaskTemplate | 2 | /api/v1/task-templates/${encodeURIComponent(editingTemplate.id)} (+1) |
| PUT | `PUT /api/v1/task-templates/{templateId}` | mux | handleUpdateTaskTemplate | 2 | /api/v1/task-templates/${encodeURIComponent(editingTemplate.id)} (+1) |
| POST | `POST /api/v1/tasks/archive` | mux | handlePostArchiveTask | 1 | /api/v1/tasks/archive |
| POST | `POST /api/v1/tasks/cancel` | mux | handlePostCancelTask | 1 | /api/v1/tasks/cancel |
| POST | `POST /api/v1/tasks/delete` | mux | handlePostDeleteTask | 1 | /api/v1/tasks/delete |
| PUT | `PUT /api/v1/tasks/update` | mux | handlePutUpdateTask | 1 | /api/v1/tasks/update |
| GET | `GET /api/v1/tasks/{project}/{agent}/{taskId}/comments` | mux | handleGetComments | 1 | /api/v1/tasks/${encodeURIComponent(project)}/${encodeURIComponent(agent)}/${encodeURIComponent(taskId)}/comments |
| POST | `POST /api/v1/tasks/{project}/{agent}/{taskId}/comments` | mux | handlePostComment | 1 | /api/v1/tasks/${encodeURIComponent(project)}/${encodeURIComponent(agent)}/${encodeURIComponent(taskId)}/comments |
| DELETE | `DELETE /api/v1/tasks/{project}/{agent}/{taskId}/comments/{commentId}` | mux | handleDeleteComment | 1 | /api/v1/tasks/${encodeURIComponent(project)}/${encodeURIComponent(agent)}/${encodeURIComponent(taskId)}/comments{VARSEP}/${encodeURIComponent(commentId)} |
| GET | `GET /api/v1/team-templates` | mux | handleTeamTemplates | 1 | /api/v1/team-templates |
| GET | `GET /api/v1/teams` | mux | handleTeams | 1 | /api/v1/teams |
| POST | `POST /api/v1/teams` | mux | handleCreateTeam | 1 | /api/v1/teams |
| POST | `POST /api/v1/teams/skills` | mux | handlePostTeamSkillBind | 3 | /api/v1/teams/${apiTeamPath(team)} (+2) |
| DELETE | `DELETE /api/v1/teams/{teamPath...}` | mux | handleDeleteTeam | 3 | /api/v1/teams/${apiTeamPath(team)} (+2) |
| GET | `GET /api/v1/teams/{teamPath...}` | mux | handleTeamDetail | 3 | /api/v1/teams/${apiTeamPath(team)} (+2) |
| PATCH | `PATCH /api/v1/teams/{teamPath...}` | mux | handleUpdateTeam | 3 | /api/v1/teams/${apiTeamPath(team)} (+2) |
| DELETE | `DELETE /api/v1/teams/{team}/roles/{role}` | mux | handleDeleteRole | 1 | /api/v1/teams/${encodeURIComponent(teamPath)}/roles/${encodeURIComponent(roleKey)} |
| PATCH | `PATCH /api/v1/teams/{team}/roles/{role}` | mux | handleUpdateRole | 1 | /api/v1/teams/${encodeURIComponent(teamPath)}/roles/${encodeURIComponent(roleKey)} |
| GET | `GET /api/v1/telemetry/log` | mux | handleTelemetryLog | 2 | /api/v1/telemetry/log?path=${encodeURIComponent(activeRun.logPath)} (+1) |
| GET | `GET /api/v1/telemetry/runs` | mux | handleTelemetryRuns | 3 | /api/v1/telemetry/runs?${p} (+2) |
| GET | `GET /api/v1/telemetry/summary` | mux | handleTelemetrySummary | 2 | /api/v1/telemetry/summary?${p} (+1) |
| GET | `GET /api/v1/user/im-identities` | mux | handleUserIMIdentities | 1 | /api/v1/user/im-identities |
| DELETE | `DELETE /api/v1/user/im-identities/connections/{connectionId}` | mux | handleUserIMConnectionUnbind | 1 | /api/v1/user/im-identities/connections/${encodeURIComponent(c.id)} |
| POST | `POST /api/v1/user/im-identities/connections/{connectionId}/bind-code` | mux | handleUserIMConnectionBindCode | 1 | /api/v1/user/im-identities/connections/${encodeURIComponent(conn.id)}/bind-code |
| GET | `GET /api/v1/users` | mux | handleListUsers | 1 | /api/v1/users |
| POST | `POST /api/v1/users` | mux | handleCreateUser | 1 | /api/v1/users |
| GET | `GET /api/v1/users/lookup` | mux | handleLookupUserByEmail | 6 | /api/v1/users/${encodeURIComponent(accessEditing.username)} (+5) |
| DELETE | `DELETE /api/v1/users/{username}` | mux | handleDeleteUser | 6 | /api/v1/users/${encodeURIComponent(accessEditing.username)} (+5) |
| GET | `GET /api/v1/users/{username}` | mux | handleGetUser | 6 | /api/v1/users/${encodeURIComponent(accessEditing.username)} (+5) |
| PUT | `PUT /api/v1/users/{username}` | mux | handleUpdateUser | 6 | /api/v1/users/${encodeURIComponent(accessEditing.username)} (+5) |
| PUT | `PUT /api/v1/users/{username}/workspace-role` | mux | handleUpdateWorkspaceMemberRole | 1 | /api/v1/users/${encodeURIComponent(accessEditing.username)}/workspace-role |
| GET | `GET /api/v1/workbench/messages` | mux | handleWorkbenchMessages | 1 | /api/v1/workbench/messages?direction=inbox&read=unread |
| GET | `GET /api/v1/workbench/overview` | mux | handleWorkbenchOverview | 1 | /api/v1/workbench/overview |
| GET | `GET /api/v1/workbench/tasks` | mux | handleWorkbenchTasks | 1 | /api/v1/workbench/tasks |
| GET | `GET /api/v1/workflow-templates` | mux | handleListWorkflowTemplates | 1 | /api/v1/workflow-templates?locale=${encodeURIComponent(templateLocale)} |
| GET | `GET /api/v1/workflows` | mux | handleListWorkflows | 1 | /api/v1/workflows |
| POST | `POST /api/v1/workflows` | mux | handleCreateWorkflow | 1 | /api/v1/workflows |
| DELETE | `DELETE /api/v1/workflows/{workflowId}` | mux | handleDeleteWorkflow | 4 | /api/v1/workflows/${encodeURIComponent(nextDraft.id)} (+3) |
| GET | `GET /api/v1/workflows/{workflowId}` | mux | handleGetWorkflow | 4 | /api/v1/workflows/${encodeURIComponent(nextDraft.id)} (+3) |
| PUT | `PUT /api/v1/workflows/{workflowId}` | mux | handleUpdateWorkflow | 4 | /api/v1/workflows/${encodeURIComponent(nextDraft.id)} (+3) |
| GET | `GET /api/v1/workspace` | mux | handleWorkspace | 1 | /api/v1/workspace |
| PUT | `PUT /api/v1/workspace` | mux | handlePutWorkspace | 1 | /api/v1/workspace |
| GET | `GET /api/v1/workspaces` | mux | handleListWorkspaces | 1 | /api/v1/workspaces |
| POST | `POST /api/v1/workspaces` | mux | handleCreateWorkspace | 1 | /api/v1/workspaces |
| POST | `POST /api/v1/workspaces/example` | mux | handleCreateExampleWorkspace | 2 | /api/v1/workspaces/${encodeURIComponent(state.data.id)} (+1) |
| DELETE | `DELETE /api/v1/workspaces/{id}` | mux | handleDeleteWorkspace | 2 | /api/v1/workspaces/${encodeURIComponent(state.data.id)} (+1) |
| POST | `POST /api/v1/workspaces/{id}/switch` | mux | handleSwitchWorkspace | 1 | /api/v1/workspaces/${encodeURIComponent(id)}/switch |
| POST | `POST /api/v1/auth/login` | publicMux | handleLogin | 1 | /api/v1/auth/login |
| POST | `POST /api/v1/auth/register` | publicMux | handleRegister | 1 | /api/v1/auth/register |
| GET | `GET /api/v1/auth/settings/public` | publicMux | handlePublicAuthSettings | 1 | /api/v1/auth/settings/public |
| GET | `GET /api/v1/invitations/{token}` | publicMux | handlePublicInvitation | 1 | /api/v1/invitations/${encodeURIComponent(inviteToken)} |
| POST | `POST /api/v1/invitations/{token}/accept` | publicMux | handleAcceptInvitation | 1 | /api/v1/invitations/${encodeURIComponent(inviteToken)}/accept |
| POST | `POST /api/v1/invitations/{token}/reject` | publicMux | handleRejectInvitation | 1 | /api/v1/invitations/${encodeURIComponent(inviteToken)}/reject |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/chat` | publicMux | handlePostTaskPreviewChat | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/chat |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/status` | publicMux | handleGetTaskPreviewStatus | 1 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/status |
| POST | `POST /api/v1/projects/{name}/tasks/{taskId}/preview/stop` | publicMux | handlePostTaskPreviewStop | 2 | /api/v1/projects/${encodeURIComponent(project \|\| 'current')}/tasks/${encodeURIComponent(taskId)}/preview/stop (+1) |

## platform-internal（74）

| method | pattern | mux | handler | 命中字面量数 | 示例 |
|---|---|---|---|---|---|
| GET | `GET /api/v1/im/instances` | mux | handleListIMInstances | 2 | /api/v1/im/instances (+1) |
| POST | `POST /api/v1/im/instances` | mux | handleCreateIMInstance | 2 | /api/v1/im/instances (+1) |
| DELETE | `DELETE /api/v1/im/instances/{id}` | mux | handleDeleteIMInstance | 0 |  |
| DELETE | `DELETE /api/v1/im/instances/{id}/connections/{connectionId}` | mux | handleDetachIMInstanceConnection | 2 | /api/v1/im/instances/${encodeURIComponent(channel.imInstanceId)}/connections/${encodeURIComponent(channel.connectionId)} (+1) |
| PUT | `PUT /api/v1/im/instances/{id}/connections/{connectionId}` | mux | handleAttachIMInstanceConnection | 2 | /api/v1/im/instances/${encodeURIComponent(channel.imInstanceId)}/connections/${encodeURIComponent(channel.connectionId)} (+1) |
| GET | `GET /api/v1/oauth/callback` | mux | handleCompleteOAuthAuthorization | 0 |  |
| * | `/preview/` | mux | handleTaskPreviewProxy | 0 |  |
| * | `/` | publicMux | func | 316 | /api/v1/agency (+315) |
| * | `/_next/` | publicMux | handleDesignRootAsset | 0 |  |
| * | `/api/` | publicMux | func | 316 | /api/v1/agency (+315) |
| * | `/api/v1/context/ingest` | publicMux | withContextIngestAuth | 0 |  |
| GET | `GET /api/v1/deploy-verify` | publicMux | handleDeployVerify | 0 |  |
| GET | `GET /api/v1/health` | publicMux | handleHealth | 1 | /api/v1/health |
| POST | `POST /api/v1/im/mattermost/actions` | publicMux | handleMattermostActionCallback | 0 |  |
| POST | `POST /api/v1/im/mattermost/commands/bind` | publicMux | handleMattermostSlashBind | 0 |  |
| POST | `POST /api/v1/im/mattermost/dialog-submit` | publicMux | handleMattermostDialogSubmit | 0 |  |
| * | `/api/v1/runtime-node/` | publicMux | withRuntimeNodeAuth | 0 |  |
| * | `/api/v1/runtime/` | publicMux | withRuntimeAgentAuth | 0 |  |
| GET | `GET /api/v1/version` | publicMux | handleHealth | 0 |  |
| * | `/design-systems/` | publicMux | handleDesignRootAsset | 0 |  |
| * | `/fonts/` | publicMux | handleDesignRootAsset | 0 |  |
| * | `/preview/` | publicMux | handleTaskPreviewProxy | 0 |  |
| * | `/projects/` | publicMux | func | 0 |  |
| POST | `POST /api/v1/runtime/actions` | runtimeMux | handleRuntimeActionProxy | 0 |  |
| GET | `GET /api/v1/runtime/attention` | runtimeMux | handleRuntimeAttentionSignals | 0 |  |
| PATCH | `PATCH /api/v1/runtime/attention/{id}` | runtimeMux | handleRuntimePatchAttentionSignal | 0 |  |
| GET | `GET /api/v1/runtime/attention/{id}/attachments/{index}` | runtimeMux | handleRuntimeAttentionAttachmentDownload | 0 |  |
| POST | `POST /api/v1/runtime/brownfield/evaluate` | runtimeMux | handleRuntimeBrownfieldEvaluate | 0 |  |
| GET | `GET /api/v1/runtime/channels` | runtimeMux | handleRuntimeChannels | 0 |  |
| POST | `POST /api/v1/runtime/ci-ready` | runtimeMux | handleRuntimeCIReady | 0 |  |
| GET | `GET /api/v1/runtime/connections` | runtimeMux | handleRuntimeConnections | 0 |  |
| GET | `GET /api/v1/runtime/contacts` | runtimeMux | handleRuntimeContacts | 0 |  |
| GET | `GET /api/v1/runtime/context` | runtimeMux | handleRuntimeContextList | 0 |  |
| GET | `GET /api/v1/runtime/context-items` | runtimeMux | handleRuntimeContextCenterItems | 0 |  |
| GET | `GET /api/v1/runtime/context-items/{id}` | runtimeMux | handleRuntimeContextCenterItemGet | 0 |  |
| GET | `GET /api/v1/runtime/context/{id}` | runtimeMux | handleRuntimeContextGet | 0 |  |
| GET | `GET /api/v1/runtime/docs` | runtimeMux | handleRuntimeDocsList | 0 |  |
| POST | `POST /api/v1/runtime/docs` | runtimeMux | handleRuntimeDocsCreate | 0 |  |
| GET | `GET /api/v1/runtime/docs/{id}` | runtimeMux | handleRuntimeDocsGet | 0 |  |
| POST | `POST /api/v1/runtime/mcp` | runtimeMux | handleRuntimeMCPProxy | 0 |  |
| POST | `POST /api/v1/runtime/mcp/gateway` | runtimeMux | handleRuntimeMCPGateway | 0 |  |
| GET | `GET /api/v1/runtime/messages` | runtimeMux | handleRuntimeMessages | 0 |  |
| POST | `POST /api/v1/runtime/messages` | runtimeMux | handleRuntimePostMessage | 0 |  |
| POST | `POST /api/v1/runtime/messages/{id}/reply` | runtimeMux | handleRuntimeReplyMessage | 0 |  |
| POST | `POST /api/v1/runtime/notify` | runtimeMux | handleRuntimeNotify | 0 |  |
| POST | `POST /api/v1/runtime/notify/file` | runtimeMux | handleRuntimeNotifyFile | 0 |  |
| POST | `POST /api/v1/runtime/notify/reaction` | runtimeMux | handleRuntimeNotifyReaction | 0 |  |
| GET | `GET /api/v1/runtime/sessions` | runtimeMux | handleRuntimeSessions | 0 |  |
| POST | `POST /api/v1/runtime/sessions` | runtimeMux | handleRuntimeSessions | 0 |  |
| GET | `GET /api/v1/runtime/sessions/{id}` | runtimeMux | handleRuntimeSession | 0 |  |
| PATCH | `PATCH /api/v1/runtime/sessions/{id}` | runtimeMux | handleRuntimeSession | 0 |  |
| POST | `POST /api/v1/runtime/skills/publish` | runtimeMux | handleRuntimeSkillPublish | 0 |  |
| GET | `GET /api/v1/runtime/task-templates` | runtimeMux | handleRuntimeTaskTemplates | 0 |  |
| GET | `GET /api/v1/runtime/tasks` | runtimeMux | handleRuntimeTasks | 0 |  |
| POST | `POST /api/v1/runtime/tasks` | runtimeMux | handleRuntimePostTask | 0 |  |
| POST | `POST /api/v1/runtime/tasks/from-template` | runtimeMux | handleRuntimePostTaskFromTemplate | 0 |  |
| GET | `GET /api/v1/runtime/tasks/{id}` | runtimeMux | handleRuntimeTask | 0 |  |
| PUT | `PUT /api/v1/runtime/tasks/{id}` | runtimeMux | handleRuntimePutTask | 0 |  |
| POST | `POST /api/v1/runtime/tasks/{id}/complete` | runtimeMux | handleRuntimeTaskComplete | 0 |  |
| POST | `POST /api/v1/runtime/tasks/{id}/confirm-request` | runtimeMux | handleRuntimeTaskConfirmRequest | 0 |  |
| GET | `GET /api/v1/runtime/tasks/{id}/workflow` | runtimeMux | handleRuntimeTaskWorkflow | 0 |  |
| POST | `POST /api/v1/runtime/tasks/{id}/workflow/branch/complete` | runtimeMux | handleRuntimeWorkflowBranchComplete | 0 |  |
| POST | `POST /api/v1/runtime/tasks/{id}/workflow/step/complete` | runtimeMux | handleRuntimeWorkflowStepComplete | 0 |  |
| POST | `POST /api/v1/runtime/workflow/decision` | runtimeMux | handleRuntimeWorkflowDecision | 0 |  |
| GET | `GET /api/v1/runtime/workflow/pending-reviews` | runtimeMux | handleRuntimeWorkflowPendingReviews | 0 |  |
| GET | `GET /api/v1/runtime-node/assets/{sha256}` | runtimeNodeMux | handleRuntimeNodeAssetDownload | 0 |  |
| POST | `POST /api/v1/runtime-node/capabilities` | runtimeNodeMux | handleRuntimeNodeHeartbeat | 0 |  |
| POST | `POST /api/v1/runtime-node/heartbeat` | runtimeNodeMux | handleRuntimeNodeHeartbeat | 0 |  |
| POST | `POST /api/v1/runtime-node/register` | runtimeNodeMux | handleRuntimeNodeRegister | 0 |  |
| POST | `POST /api/v1/runtime-node/runs/claim` | runtimeNodeMux | handleRuntimeNodeClaimRun | 0 |  |
| POST | `POST /api/v1/runtime-node/runs/{runId}/complete` | runtimeNodeMux | handleRuntimeNodeRunComplete | 0 |  |
| POST | `POST /api/v1/runtime-node/runs/{runId}/fail` | runtimeNodeMux | handleRuntimeNodeRunFail | 0 |  |
| POST | `POST /api/v1/runtime-node/runs/{runId}/lease` | runtimeNodeMux | handleRuntimeNodeRunLease | 0 |  |
| GET | `GET /api/v1/runtime-node/runs/{runId}/spec` | runtimeNodeMux | handleRuntimeNodeRunSpec | 0 |  |

## websocket-surface（6）

| method | pattern | mux | handler | 命中字面量数 | 示例 |
|---|---|---|---|---|---|
| GET | `GET /api/v1/audit/events` | mux | handleAuditEvents | 0 |  |
| GET | `GET /api/v1/projects/{name}/agents/{agent}/live-log` | mux | handleAgentLiveLog | 2 | /api/v1/projects/${encodeURIComponent(projectId)}/agents/${encodeURIComponent(agentName)}/live-log (+1) |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/live` | mux | handleGetTaskPreviewLive | 0 |  |
| POST | `POST /api/v1/im/{provider}/events` | publicMux | handleIMEvent | 0 |  |
| GET | `GET /api/v1/projects/{name}/tasks/{taskId}/preview/live` | publicMux | handleGetTaskPreviewLive | 0 |  |
| POST | `POST /api/v1/runtime-node/runs/{runId}/events` | runtimeNodeMux | handleRuntimeNodeRunEvent | 0 |  |

## 前端字面量未命中任何注册（证据完备性）

这些是前端出现的 /api/v1 字符串，但按本脚本的保守匹配规则没有对应注册端点。
裁剪裁决前应人工过目（多为完整查询串拼接或跨段拼接）。



## 数据质量附注（夜间机械提取的已知边界）

1. **动态动词分发**：`/deploy/requests/${id}/${action}`（action=approve|cancel|reject）按 dynamic-verb 记账，命中其全部兄弟路由——此类端点的"零调用"结论永远不可下，因为一个调用点可覆盖多个端点（81 个 dynamic-verb 命中）。
2. **变量组合**：一级 const 展开（designBase 模式）已处理；两级以上组合或运行时拼接的调用点会漏报（偏保守方向：zero-call 高估）。design/chat 与 design/launch 经人工复核无 web 调用点（chat 后端注释标记 A.5-3，launch 由 start 响应的 launchUrl 直达 OD，不经 web fetch）。
3. **非 web 调用方**：mga CLI 走 runtimeMux/runtimeNodeMux（platform-internal 档）；IM 回调走 publicMux /api/v1/im/*；health/version/deploy-verify 为平台内部或自证端点。
4. **匹配规则**：${...} 即一个参数段；字面量段数必须与路由对齐（或以 / 结尾表深路径构建）。v3 §7.2 的"前端实际调用 130"在本口径下对应 exact+query+dynamic-verb 命中的路由数；两口径差异见 deviations.md DEV-6。
5. **本底稿无任何保留/废弃结论**——裁剪裁决是 STOP 点。
