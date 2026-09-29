package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/spf13/cobra"
)

func newChannelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "channel",
		Short: "查看渠道与配置凭据",
	}
	cmd.AddCommand(newChannelListCmd(), newChannelSetCmd(), newChannelToggleCmd())
	return cmd
}

func newChannelListCmd() *cobra.Command {
	var app string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出支持的渠道及其所需参数",
		RunE: func(cmd *cobra.Command, _ []string) error {
			var cfg *config.AppConfig
			if app != "" {
				applicationID, err := artifact.ValidateApplicationID(app)
				if err != nil {
					return err
				}
				loaded, found, err := config.NewStore("").Get(applicationID)
				if err != nil {
					return err
				}
				if found {
					cfg = &loaded
				}
			}

			if jsonOut {
				type paramJSON struct {
					Name        string `json:"name"`
					Description string `json:"description"`
					Required    bool   `json:"required"`
					Type        string `json:"type"`
					EnvName     string `json:"envName"`
					Configured  *bool  `json:"configured"`
				}
				type capJSON struct {
					SupportedStages               []string `json:"supportedStages"`
					RiskLevel                     string   `json:"riskLevel"`
					RiskLevelLabel                string   `json:"riskLevelLabel"`
					Withdrawal                    string   `json:"withdrawal"`
					WithdrawalLabel               string   `json:"withdrawalLabel"`
					RequiresExplicitConfirmation  bool     `json:"requiresExplicitConfirmation"`
					AutomaticRetryAfterSubmission bool     `json:"automaticRetryAfterSubmission"`
					Evidence                      string   `json:"evidence"`
					EvidenceLabel                 string   `json:"evidenceLabel"`
					VerifiedScope                 *string  `json:"verifiedScope"`
					Note                          string   `json:"note"`
				}
				type channelJSON struct {
					ID          string      `json:"id"`
					DisplayName string      `json:"displayName"`
					FileNameTag string      `json:"fileNameTag"`
					Capability  capJSON     `json:"capability"`
					Params      []paramJSON `json:"params"`
				}
				out := struct {
					OK       bool          `json:"ok"`
					Channels []channelJSON `json:"channels"`
				}{OK: true}

				for _, ch := range channel.All() {
					caps := ch.Capabilities()
					item := channelJSON{
						ID: ch.ID(), DisplayName: ch.DisplayName(),
						FileNameTag: ch.FileNameTag(),
						Capability: capJSON{
							RiskLevel:                     caps.RiskLevel.String(),
							RiskLevelLabel:                caps.RiskLevel.Label(),
							Withdrawal:                    caps.Withdrawal.String(),
							WithdrawalLabel:               caps.Withdrawal.Label(),
							RequiresExplicitConfirmation:  caps.RequiresExplicitConfirmation,
							AutomaticRetryAfterSubmission: caps.AutomaticRetryAfterSubmission,
							Evidence:                      caps.Evidence.String(),
							EvidenceLabel:                 caps.Evidence.Label(),
							Note:                          caps.Note,
						},
					}
					for _, s := range caps.SupportedStages {
						item.Capability.SupportedStages = append(item.Capability.SupportedStages, s.String())
					}
					if caps.VerifiedScope != "" {
						scope := caps.VerifiedScope
						item.Capability.VerifiedScope = &scope
					}
					for _, p := range ch.Params() {
						pj := paramJSON{
							Name: p.Name, Description: p.Description,
							Required: p.Required, Type: paramTypeName(p),
							EnvName: config.EnvName(ch.ID(), p.Name),
						}
						if cfg != nil {
							v := configuredIn(cfg, ch.ID(), p.Name)
							pj.Configured = &v
						}
						item.Params = append(item.Params, pj)
					}
					out.Channels = append(out.Channels, item)
				}
				return output.JSON(out)
			}

			// 先输出能力画像：风险等级与「能不能停在送审之前」决定了该怎么用这个渠道，
			// 应该在动手之前就看到，而不是发完才发现没法反悔
			rows := make([][]string, 0)
			for _, ch := range channel.All() {
				caps := ch.Capabilities()
				rows = append(rows, []string{
					ch.ID(), ch.DisplayName(),
					caps.RiskLevel.Label(),
					describeStages(caps.SupportedStages),
					caps.Withdrawal.Label(),
					caps.Evidence.Label(),
				})
			}
			output.Table([]string{"渠道", "名称", "风险", "可停在", "撤回", "证据"}, rows)
			output.Line("")
			for _, ch := range channel.All() {
				caps := ch.Capabilities()
				output.Line("%s（%s）：%s", ch.DisplayName(), ch.ID(), caps.Note)
				if caps.VerifiedScope != "" {
					output.Line("    实测范围：%s", caps.VerifiedScope)
				}
			}
			output.Line("")
			output.Line("所需参数：")
			paramRows := make([][]string, 0)
			for _, ch := range channel.All() {
				for _, p := range ch.Params() {
					state := "-"
					if cfg != nil {
						switch {
						case configuredIn(cfg, ch.ID(), p.Name):
							state = "已配置"
						case os.Getenv(config.EnvName(ch.ID(), p.Name)) != "":
							state = "环境变量"
						default:
							state = "缺失"
						}
					}
					paramRows = append(paramRows, []string{
						ch.ID(), ch.DisplayName(), p.Name, p.Description, state,
					})
				}
			}
			output.Table([]string{"渠道", "名称", "参数", "说明", "状态"}, paramRows)
			output.Line("")
			output.Line("凭据也可用环境变量提供，优先于配置文件，例如：")
			output.Line("  export %s=xxx", config.EnvName("huawei", "client_secret"))
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "指定应用后额外显示各参数是否已配置")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	return cmd
}

func newChannelSetCmd() *cobra.Command {
	var (
		app       string
		channelID string
		key       string
		value     string
		valueFile string
		jsonOut   bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "设置渠道凭据",
		Long: "设置渠道凭据。\n\n" +
			"凭据以 600 权限保存在 ~/.easy-publisher/apps/ 下。注意用 --value 传入的值会进入\n" +
			"shell 历史，敏感凭据建议用 --value-file 从文件读取，或改用环境变量。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(app)
			if err != nil {
				return err
			}
			target, err := channel.Require(channelID)
			if err != nil {
				return err
			}
			param, ok := findParam(target, key)
			if !ok {
				names := make([]string, 0, len(target.Params()))
				for _, p := range target.Params() {
					names = append(names, p.Name)
				}
				return eperr.ConfigurationError(
					"渠道 %s 没有参数 %s。可用参数：%s",
					target.DisplayName(), key, strings.Join(names, ", "))
			}

			resolved, err := resolveValue(param, value, valueFile)
			if err != nil {
				return err
			}

			store := config.NewStore("")
			cfg, err := store.Require(applicationID)
			if err != nil {
				return err
			}
			existing, ok := cfg.Channel(target.ID())
			if !ok {
				existing = config.ChannelConfig{Name: target.ID(), Enabled: true}
			}
			if err := store.Save(cfg.WithChannel(existing.WithParam(param.Name, resolved))); err != nil {
				return err
			}

			if jsonOut {
				return output.JSON(map[string]any{
					"ok": true, "channel": target.ID(), "key": param.Name,
				})
			}
			// 绝不回显参数值
			output.Line("已保存 %s 的 %s", target.DisplayName(), param.Name)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "包名（必填）")
	cmd.Flags().StringVar(&channelID, "channel", "", "渠道 id（必填）")
	cmd.Flags().StringVar(&key, "key", "", "参数名（必填）")
	cmd.Flags().StringVar(&value, "value", "", "参数值")
	cmd.Flags().StringVar(&valueFile, "value-file", "",
		"从文件读取参数值（小米的公钥证书等文件型参数必须用这个）")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	for _, flag := range []string{"app", "channel", "key"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

func newChannelToggleCmd() *cobra.Command {
	var app, channelID string
	var enable, disable, jsonOut bool
	cmd := &cobra.Command{
		Use:   "toggle",
		Short: "启用或停用某个渠道",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if enable == disable {
				return eperr.ConfigurationError("必须指定 --enable 或 --disable 其中之一")
			}
			applicationID, err := artifact.ValidateApplicationID(app)
			if err != nil {
				return err
			}
			target, err := channel.Require(channelID)
			if err != nil {
				return err
			}
			store := config.NewStore("")
			cfg, err := store.Require(applicationID)
			if err != nil {
				return err
			}
			existing, ok := cfg.Channel(target.ID())
			if !ok {
				existing = config.ChannelConfig{Name: target.ID()}
			}
			existing.Enabled = enable
			if err := store.Save(cfg.WithChannel(existing)); err != nil {
				return err
			}

			if jsonOut {
				return output.JSON(map[string]any{
					"ok": true, "channel": target.ID(), "enabled": enable,
				})
			}
			state := "停用"
			if enable {
				state = "启用"
			}
			output.Line("%s 已%s", target.DisplayName(), state)
			return nil
		},
	}
	cmd.Flags().StringVar(&app, "app", "", "包名（必填）")
	cmd.Flags().StringVar(&channelID, "channel", "", "渠道 id（必填）")
	cmd.Flags().BoolVar(&enable, "enable", false, "启用")
	cmd.Flags().BoolVar(&disable, "disable", false, "停用")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	for _, flag := range []string{"app", "channel"} {
		_ = cmd.MarkFlagRequired(flag)
	}
	return cmd
}

// ---- 辅助 ----

func findParam(ch channel.Channel, name string) (channel.ChannelParam, bool) {
	for _, p := range ch.Params() {
		if strings.EqualFold(p.Name, name) {
			return p, true
		}
	}
	return channel.ChannelParam{}, false
}

func resolveValue(param channel.ChannelParam, value, valueFile string) (string, error) {
	switch {
	case valueFile != "" && value != "":
		return "", eperr.ConfigurationError("--value 与 --value-file 只能用一个")
	case valueFile != "":
		data, err := os.ReadFile(valueFile)
		if err != nil {
			return "", eperr.LocalFileError("无法读取 %s：%v", valueFile, err)
		}
		if param.Type == channel.ParamTextFile && param.FileExtension != "" {
			if !strings.HasSuffix(strings.ToLower(valueFile), "."+param.FileExtension) {
				fmt.Fprintf(os.Stderr, "提示：%s 通常是 .%s 文件，当前传入的是 %s\n",
					param.Name, param.FileExtension, valueFile)
			}
		}
		return strings.TrimSpace(string(data)), nil
	case value != "":
		return value, nil
	default:
		return "", eperr.ConfigurationError("必须提供 --value 或 --value-file")
	}
}

func paramTypeName(p channel.ChannelParam) string {
	if p.Type == channel.ParamTextFile {
		return "file(." + p.FileExtension + ")"
	}
	return "text"
}

func describeStages(stages []channel.ReleaseStage) string {
	parts := make([]string, 0, len(stages))
	for _, s := range stages {
		parts = append(parts, s.ShortLabel())
	}
	return strings.Join(parts, "/")
}

// configuredIn 报告配置文件里是否已设置该参数。
//
// 空字符串视为未配置 —— 与 config 包的语义一致。
func configuredIn(cfg *config.AppConfig, channelID, param string) bool {
	if cfg == nil {
		return false
	}
	ch, ok := cfg.Channel(channelID)
	if !ok {
		return false
	}
	_, ok = ch.Value(param)
	return ok
}
