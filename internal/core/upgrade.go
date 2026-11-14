package core

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/rshun/grok-bot/internal/grok"
	"github.com/rshun/grok-bot/internal/platform"
	"github.com/rshun/grok-bot/internal/upgrade"
)

const pendingUpgradeNoticeKey = "pending_upgrade_notice"

// CLIUpgrader is an optional extension implemented by *grok.Client for Grok CLI self-updates.
type CLIUpgrader interface {
	Version(ctx context.Context) (string, error)
	CheckUpdatePreflight() (string, error)
	UpdateCLI(ctx context.Context) (grok.CLIUpdateResult, error)
}

// UpgradeNotice records an in-flight grok-bot upgrade across process restarts.
type UpgradeNotice struct {
	Platform    string `json:"platform"`
	ChatID      string `json:"chat_id"`
	FromVersion string `json:"from_version"`
	ToVersion   string `json:"to_version"`
	CLIUpdated  string `json:"cli_updated,omitempty"`
	CreatedAt   string `json:"created_at"`
}

const upgradeUsage = `用法：
/upgrade
    同时检查 grok-bot 与 Grok CLI 版本状态（附一键升级按钮）
/upgrade bot
    单独检查并准备升级 grok-bot 机器人自身
/upgrade cli
    单独检查并准备更新本机官方 Grok CLI 引擎`

func (s *Service) checkUpgradeAdmin(in platform.Inbound) (bool, string) {
	hasExplicitAdmins := false
	for _, admins := range s.AdminUsers {
		if len(admins) > 0 {
			hasExplicitAdmins = true
			break
		}
	}
	if hasExplicitAdmins {
		if _, ok := s.AdminUsers[in.Platform][in.UserID]; ok {
			return true, ""
		}
		return false, "没有权限：仅管理员可执行版本升级。"
	}
	for _, allowed := range s.AllowedUsers {
		if len(allowed) > 1 {
			return false, "当前配置了多个白名单用户，请先在环境配置文件中设置 ADMIN_USER_IDS（或 DISCORD_ADMIN_USER_IDS）指定管理员后再执行升级。"
		}
	}
	return true, ""
}

func (s *Service) botUpgrader() *upgrade.Upgrader {
	if s.Upgrader != nil {
		return s.Upgrader
	}
	return &upgrade.Upgrader{
		CurrentVersion: s.Version,
		Paths:          upgrade.SnapshotExecutable(),
		DataDir:        s.dataDirGuess(),
	}
}

func (s *Service) dataDirGuess() string {
	if s.BackupDir != "" {
		return filepath.Dir(s.BackupDir)
	}
	if s.UploadDir != "" {
		return filepath.Dir(s.UploadDir)
	}
	return ""
}

func (s *Service) backupDir() string {
	if strings.TrimSpace(s.BackupDir) != "" {
		return s.BackupDir
	}
	if base := s.dataDirGuess(); base != "" {
		return filepath.Join(base, "backups")
	}
	return "backups"
}

func (s *Service) drainTimeout() time.Duration {
	if s.DrainTimeout > 0 {
		return s.DrainTimeout
	}
	return 60 * time.Second
}

func (s *Service) countOtherBusyLanes(excludePlatform, excludeChatID string) int {
	curKey := excludePlatform + "\x00" + excludeChatID
	s.mu.Lock()
	lanes := make(map[string]*lane, len(s.lanes))
	for k, v := range s.lanes {
		lanes[k] = v
	}
	s.mu.Unlock()

	busy := 0
	for key, ln := range lanes {
		ln.mu.Lock()
		d := ln.depth
		ln.mu.Unlock()
		if key == curKey {
			if d > 1 {
				busy++
			}
			continue
		}
		if d > 0 {
			busy++
		}
	}
	return busy
}

func (s *Service) waitForOtherLanes(ctx context.Context, excludePlatform, excludeChatID string) (bool, int) {
	rem := s.countOtherBusyLanes(excludePlatform, excludeChatID)
	if rem == 0 {
		return true, 0
	}
	deadline := time.Now().Add(s.drainTimeout())
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return false, s.countOtherBusyLanes(excludePlatform, excludeChatID)
		case <-ticker.C:
			rem = s.countOtherBusyLanes(excludePlatform, excludeChatID)
			if rem == 0 {
				return true, 0
			}
			if time.Now().After(deadline) {
				return false, rem
			}
		}
	}
}

func (s *Service) upgradeCommand(ctx context.Context, in platform.Inbound, out platform.Replier, arg string) (string, [][]platform.Button, error) {
	if ok, denyMsg := s.checkUpgradeAdmin(in); !ok {
		return denyMsg, nil, nil
	}
	fields := strings.Fields(strings.TrimSpace(arg))
	if len(fields) == 0 {
		return s.upgradePanel(ctx, "")
	}
	sub := strings.ToLower(fields[0])
	switch sub {
	case "help":
		return upgradeUsage, nil, nil
	case "check", "status", "all":
		return s.upgradePanel(ctx, "")
	case "bot", "grok-bot", "self":
		return s.upgradePanel(ctx, "bot")
	case "cli", "grok", "engine":
		return s.upgradePanel(ctx, "cli")
	case "cancel":
		return "已取消升级操作。", nil, nil
	case "confirm":
		if len(fields) < 2 {
			return upgradeUsage, nil, nil
		}
		target := strings.ToLower(fields[1])
		expectedTag := ""
		if len(fields) >= 3 {
			expectedTag = fields[2]
		}
		switch target {
		case "cli":
			return s.executeCLIUpgrade(ctx, in)
		case "bot":
			return s.executeBotUpgrade(ctx, in, out, expectedTag, false)
		case "all":
			return s.executeBotUpgrade(ctx, in, out, expectedTag, true)
		default:
			return upgradeUsage, nil, nil
		}
	default:
		return upgradeUsage, nil, nil
	}
}

func (s *Service) upgradePanel(ctx context.Context, focus string) (string, [][]platform.Button, error) {
	botVer := strings.TrimSpace(s.Version)
	if botVer == "" {
		botVer = "dev"
	}

	var b strings.Builder
	b.WriteString("📦 版本与升级检查\n\n")

	var canUpgradeBot bool
	var latestTag string
	if focus == "" || focus == "bot" {
		upg := s.botUpgrader()
		pf := upg.CheckPreflight()
		rel, relErr := upg.CheckLatest(ctx)
		fmt.Fprintf(&b, "• grok-bot 机器人：当前 `%s`", botVer)
		switch {
		case relErr != nil:
			fmt.Fprintf(&b, "\n  ⚠️ 查询远端最新版本失败：%s\n", grok.RedactSensitive(relErr.Error()))
		default:
			latestTag = rel.TagName
			cmp, cmpOK := upgrade.CompareVersions(botVer, rel.TagName)
			switch {
			case cmpOK && cmp < 0:
				fmt.Fprintf(&b, " → 最新 `%s`（可升级）\n", rel.TagName)
				if pf.Mode != upgrade.ModeUnavailable {
					canUpgradeBot = true
				}
			case cmpOK && cmp == 0:
				fmt.Fprintf(&b, "（已是最新版本）\n")
			case cmpOK && cmp > 0:
				fmt.Fprintf(&b, "（高于最新发布版 `%s`）\n", rel.TagName)
			default:
				fmt.Fprintf(&b, "（最新发布版：`%s`）\n", rel.TagName)
			}
		}
		switch pf.Mode {
		case upgrade.ModeInPlace:
			fmt.Fprintf(&b, "  升级方式：进程内 SHA256 校验原子替换（`%s`）\n", pf.TargetPath)
		case upgrade.ModeSudoHelper:
			fmt.Fprintf(&b, "  升级方式：系统受控升级脚本（`%s`）\n", pf.TargetPath)
		case upgrade.ModeUnavailable:
			fmt.Fprintf(&b, "  自动替换状态：不可用（%s）\n", pf.Reason)
			if pf.Guide != "" && (canUpgradeBot || focus == "bot" || (latestTag != "" && botVer != latestTag)) {
				b.WriteString("\n" + pf.Guide + "\n")
			}
		}
		if focus == "" {
			b.WriteString("\n")
		}
	}

	var canUpgradeCLI bool
	if focus == "" || focus == "cli" {
		cliUpg, ok := s.Engine.(CLIUpgrader)
		if !ok {
			if mcpEng, mcpOK := s.Engine.(MCPEngine); mcpOK {
				cliVer, _ := mcpEng.Version(ctx)
				if cliVer == "" {
					cliVer = "未知"
				}
				fmt.Fprintf(&b, "• Grok CLI 引擎：当前 `%s`（当前运行引擎未启用在线升级）\n", cliVer)
			} else {
				b.WriteString("• Grok CLI 引擎：不支持在线升级\n")
			}
		} else {
			cliVer, verErr := cliUpg.Version(ctx)
			if verErr != nil || cliVer == "" {
				cliVer = "未知"
			}
			fmt.Fprintf(&b, "• Grok CLI 引擎：当前 `%s`\n", cliVer)
			if targetPath, pfErr := cliUpg.CheckUpdatePreflight(); pfErr != nil {
				fmt.Fprintf(&b, "  自动更新状态：不可用（%s）\n", grok.RedactSensitive(pfErr.Error()))
			} else {
				canUpgradeCLI = true
				fmt.Fprintf(&b, "  程序路径：`%s`（权限检查通过）\n", targetPath)
			}
		}
	}

	var rows [][]platform.Button
	var actionRow []platform.Button
	if canUpgradeBot && latestTag != "" {
		cmd := "/upgrade confirm bot " + latestTag
		if len(cmd) <= 64 {
			actionRow = append(actionRow, platform.Button{
				Label:   "⬆️ 升级 grok-bot (" + latestTag + ")",
				Command: cmd,
				Style:   platform.ButtonPrimary,
			})
		}
	}
	if canUpgradeCLI {
		style := platform.ButtonSecondary
		if focus == "cli" && !canUpgradeBot {
			style = platform.ButtonPrimary
		}
		actionRow = append(actionRow, platform.Button{
			Label:   "🔄 更新 Grok CLI",
			Command: "/upgrade confirm cli",
			Style:   style,
		})
	}
	if len(actionRow) > 0 {
		rows = append(rows, actionRow)
	}
	if focus == "" && canUpgradeBot && canUpgradeCLI && latestTag != "" {
		allCmd := "/upgrade confirm all " + latestTag
		if len(allCmd) <= 64 {
			rows = append(rows, []platform.Button{
				{Label: "🚀 两者一起升级", Command: allCmd, Style: platform.ButtonSuccess},
				{Label: "取消", Command: "/upgrade cancel", Style: platform.ButtonSecondary},
			})
		}
	} else if len(rows) > 0 {
		rows = append(rows, []platform.Button{
			{Label: "取消", Command: "/upgrade cancel", Style: platform.ButtonSecondary},
		})
	}

	if len(rows) > 0 {
		b.WriteString("\n请点击下方按钮确认执行升级：")
	}
	return strings.TrimSpace(b.String()), rows, nil
}

func (s *Service) executeCLIUpgrade(ctx context.Context, in platform.Inbound) (string, [][]platform.Button, error) {
	cliUpg, ok := s.Engine.(CLIUpgrader)
	if !ok {
		return "当前运行引擎不支持 Grok CLI 在线升级。", nil, nil
	}
	if !s.upgrading.CompareAndSwap(false, true) {
		return "当前已有升级任务正在执行，请稍候。", nil, nil
	}
	defer s.upgrading.Store(false)

	if drained, rem := s.waitForOtherLanes(ctx, in.Platform, in.ChatID); !drained {
		return fmt.Sprintf("当前仍有 %d 个进行中的对话或定时任务尚未结束，为避免中断他人会话已暂缓升级，请稍后重试。", rem), nil, nil
	}

	res, err := cliUpg.UpdateCLI(ctx)
	if err != nil {
		return "❌ Grok CLI 更新失败：" + grok.RedactSensitive(err.Error()), nil, nil
	}

	var b strings.Builder
	if res.Updated {
		fmt.Fprintf(&b, "✅ Grok CLI 已更新：`%s` → `%s`（后台守护进程已刷新）。", res.BeforeVersion, res.AfterVersion)
	} else {
		ver := res.AfterVersion
		if ver == "" {
			ver = res.BeforeVersion
		}
		fmt.Fprintf(&b, "✅ Grok CLI 已是最新版本（`%s`），后台守护进程已刷新。", ver)
	}

	// Proactively verify whether the chat's active model still exists in the updated CLI.
	var btns [][]platform.Button
	if s.Store != nil {
		if chat, chatErr := s.Store.Chat(ctx, in.Platform, in.ChatID); chatErr == nil && chat.Model != "" {
			if list, mErr := s.Engine.Models(ctx); mErr == nil && len(list.Names) > 0 && !containsModel(list, chat.Model) {
				fmt.Fprintf(&b, "\n\n⚠️ 注意：当前会话使用的模型 `%s` 在新版 Grok CLI 中已下线或更名，请点选下方可用模型切换（上下文将继续保留）：", chat.Model)
				btns = modelButtons(chat.Model, list)
			}
		}
	}
	return b.String(), btns, nil
}

func (s *Service) executeBotUpgrade(ctx context.Context, in platform.Inbound, out platform.Replier, expectedTag string, includeCLI bool) (string, [][]platform.Button, error) {
	upg := s.botUpgrader()
	pf := upg.CheckPreflight()
	if pf.Mode == upgrade.ModeUnavailable {
		msg := "当前环境无法自动升级 grok-bot：" + pf.Reason
		if pf.Guide != "" {
			msg += "\n\n" + pf.Guide
		}
		return msg, nil, nil
	}

	if !s.upgrading.CompareAndSwap(false, true) {
		return "当前已有升级任务正在执行，请稍候。", nil, nil
	}
	keepLock := false
	defer func() {
		if !keepLock {
			s.upgrading.Store(false)
		}
	}()

	if drained, rem := s.waitForOtherLanes(ctx, in.Platform, in.ChatID); !drained {
		return fmt.Sprintf("当前仍有 %d 个进行中的对话或定时任务尚未结束，为避免中断他人会话已暂缓升级，请稍后重试。", rem), nil, nil
	}

	rel, err := upg.CheckLatest(ctx)
	if err != nil {
		return "❌ 检查最新版本失败：" + grok.RedactSensitive(err.Error()), nil, nil
	}
	if expectedTag != "" && !strings.EqualFold(expectedTag, rel.TagName) {
		return fmt.Sprintf("远端最新版本已变更为 `%s`（原确认版本为 `%s`），请重新发送 `/upgrade` 确认后再执行。", rel.TagName, expectedTag), nil, nil
	}
	botVer := strings.TrimSpace(s.Version)
	if cmp, ok := upgrade.CompareVersions(botVer, rel.TagName); ok && cmp >= 0 {
		return fmt.Sprintf("✅ grok-bot 当前已是最新版本（`%s`），无需升级。", botVer), nil, nil
	}

	var cliSummary string
	if includeCLI {
		if cliUpg, ok := s.Engine.(CLIUpgrader); ok {
			cliRes, cliErr := cliUpg.UpdateCLI(ctx)
			if cliErr != nil {
				return "❌ Grok CLI 更新失败，已中止整体升级：" + grok.RedactSensitive(cliErr.Error()), nil, nil
			}
			if cliRes.Updated {
				cliSummary = fmt.Sprintf("Grok CLI：%s → %s", cliRes.BeforeVersion, cliRes.AfterVersion)
			} else {
				cliSummary = fmt.Sprintf("Grok CLI：已是最新 (%s)", cliRes.AfterVersion)
			}
		}
	}

	if s.Store != nil {
		if _, bakErr := s.Store.Backup(ctx, s.backupDir(), botVer); bakErr != nil {
			return "❌ 数据库升级前备份失败，已安全中止升级：" + grok.RedactSensitive(bakErr.Error()), nil, nil
		}
		notice := UpgradeNotice{
			Platform:    in.Platform,
			ChatID:      in.ChatID,
			FromVersion: botVer,
			ToVersion:   rel.TagName,
			CLIUpdated:  cliSummary,
			CreatedAt:   s.now().UTC().Format(time.RFC3339),
		}
		if raw, mErr := json.Marshal(notice); mErr == nil {
			if putErr := s.Store.PutSetting(ctx, pendingUpgradeNoticeKey, string(raw)); putErr != nil {
				slog.Warn("save pending upgrade notice failed", "err", putErr)
			}
		}
	}

	clearNotice := func() {
		if s.Store != nil {
			_ = s.Store.DeleteSetting(context.Background(), pendingUpgradeNoticeKey)
		}
	}

	if pf.Mode == upgrade.ModeSudoHelper {
		s.send(ctx, out, in.ChatID, fmt.Sprintf("🔐 数据库已在线备份，正在调用系统受控升级脚本更新至 `%s` 并重启服务（完成后将自动推送回执）…", rel.TagName))
		keepLock = true
		if err := upg.RunSudoHelper(ctx); err != nil {
			clearNotice()
			keepLock = false
			return "❌ " + grok.RedactSensitive(err.Error()), nil, nil
		}
		return "正在等待系统服务重启切换…", nil, nil
	}

	if _, err := upg.DownloadAndApply(ctx, rel); err != nil {
		clearNotice()
		return "❌ 升级包下载或校验替换失败（原程序未受影响）：" + grok.RedactSensitive(err.Error()), nil, nil
	}

	keepLock = true
	s.restartPending.Store(true)
	msg := fmt.Sprintf("📦 grok-bot 已通过 SHA256 校验并更新至 `%s`（数据库与旧版程序已备份），正在平滑重启服务，完成后将自动通知…", rel.TagName)
	if cliSummary != "" {
		msg = "✅ " + cliSummary + "\n" + msg
	}
	return msg, nil, nil
}

// DeliverPendingUpgradeNotice checks whether the previous instance recorded an upgrade notice,
// clears it immediately, and sends a completion confirmation to the initiating admin chat.
func (s *Service) DeliverPendingUpgradeNotice(ctx context.Context) {
	if s.Store == nil {
		return
	}
	raw, ok, err := s.Store.Setting(ctx, pendingUpgradeNoticeKey)
	if err != nil || !ok || strings.TrimSpace(raw) == "" {
		return
	}
	_ = s.Store.DeleteSetting(ctx, pendingUpgradeNoticeKey)

	var notice UpgradeNotice
	if err := json.Unmarshal([]byte(raw), &notice); err != nil {
		return
	}
	if notice.Platform == "" || notice.ChatID == "" {
		return
	}
	if ts, tErr := time.Parse(time.RFC3339, notice.CreatedAt); tErr == nil {
		if s.now().Sub(ts) > 30*time.Minute {
			return
		}
	}
	out, ok := s.Senders[notice.Platform]
	if !ok || out == nil {
		return
	}

	curVer := strings.TrimSpace(s.Version)
	if curVer == "" {
		curVer = "dev"
	}
	var msg string
	if notice.ToVersion == "" || strings.EqualFold(curVer, notice.ToVersion) {
		msg = fmt.Sprintf("✅ grok-bot 升级完成：`%s` → `%s`，所有会话与定时任务已恢复正常。", notice.FromVersion, curVer)
		if notice.CLIUpdated != "" {
			msg += "\n（" + notice.CLIUpdated + "）"
		}
	} else {
		msg = fmt.Sprintf("⚠️ 服务已重启，当前运行版本为 `%s`（目标版本为 `%s`），请发送 `/status` 检查状态。", curVer, notice.ToVersion)
	}
	s.send(ctx, out, notice.ChatID, msg)
}

// parseNaturalUpgradeIntent detects short, standalone user messages asking to check or upgrade
// grok-bot or Grok CLI, returning the panel focus ("", "bot", or "cli") without auto-confirming.
func parseNaturalUpgradeIntent(text string) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" || strings.HasPrefix(trimmed, "/") {
		return "", false
	}
	if strings.ContainsAny(trimmed, "\n\r`") || utf8.RuneCountInString(trimmed) > 28 {
		return "", false
	}
	lower := strings.ToLower(trimmed)
	if strings.Contains(lower, "http://") || strings.Contains(lower, "https://") {
		return "", false
	}
	// Never intercept general technical questions or coding requests.
	for _, skip := range []string{
		"怎么", "如何", "为什么", "是什么", "帮我写", "写一个", "写个", "脚本", "代码",
		"how to", "why ", "write a", "script",
		"ubuntu", "debian", "centos", "linux", "windows", "macos", "python", "pip",
		"node", "npm", "docker", "apt", "brew", "内核", "系统包",
	} {
		if strings.Contains(lower, skip) {
			return "", false
		}
	}

	norm := strings.Trim(lower, " \t。，！？.,!?~～")
	normCompact := strings.NewReplacer(" ", "", "\t", "").Replace(norm)

	for _, cliPhrase := range []string{
		"升级cli", "更新cli", "升级grokcli", "更新grokcli", "grokcli升级", "grokcli更新",
		"升级底层引擎", "更新底层引擎", "检查cli更新",
		"upgradecli", "updatecli", "upgradegrokcli", "updategrokcli", "checkcliupdate",
	} {
		if normCompact == cliPhrase {
			return "cli", true
		}
	}
	for _, botPhrase := range []string{
		"升级机器人", "更新机器人", "机器人升级", "机器人更新",
		"升级grok-bot", "更新grok-bot", "grok-bot升级", "grok-bot更新",
		"升级grokbot", "更新grokbot", "升级bot", "更新bot", "检查机器人更新",
		"upgradebot", "updatebot", "upgradegrok-bot", "updategrok-bot", "upgradegrokbot", "updategrokbot",
	} {
		if normCompact == botPhrase {
			return "bot", true
		}
	}
	for _, allPhrase := range []string{
		"检查更新", "检查升级", "查看更新", "有没有新版本", "有新版本吗",
		"升级一下", "更新一下", "自动升级", "自我升级", "升级版本", "更新版本", "升级程序", "更新程序",
		"checkforupdates", "checkforupdate", "checkupdates", "checkupdate",
	} {
		if normCompact == allPhrase {
			return "", true
		}
	}
	return "", false
}
