package main

import (
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/artifact"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/config"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
	"github.com/fangxiangCN/easy-publisher/go/internal/output"
	"github.com/spf13/cobra"
)

// newAppCmd 构造 app 命令组。
func newAppCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "app",
		Short: "管理待发布的应用",
	}
	cmd.AddCommand(newAppListCmd(), newAppAddCmd(), newAppSetCmd(), newAppRemoveCmd())
	return cmd
}

func newAppListCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "list",
		Short: "列出已配置的应用",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store := config.NewStore("")
			apps, err := store.List()
			if err != nil {
				return err
			}

			if jsonOut {
				type channelJSON struct {
					ID      string   `json:"id"`
					Enabled bool     `json:"enabled"`
					Params  []string `json:"configured"`
				}
				type appJSON struct {
					ApplicationID   string        `json:"applicationId"`
					Name            string        `json:"name"`
					MultiChannelApk bool          `json:"multiChannelApk"`
					Channels        []channelJSON `json:"channels"`
				}
				out := struct {
					OK   bool      `json:"ok"`
					Apps []appJSON `json:"apps"`
				}{OK: true, Apps: []appJSON{}}

				for _, app := range apps {
					item := appJSON{
						ApplicationID:   app.ApplicationID,
						Name:            app.Name,
						MultiChannelApk: app.MultiChannelApk,
						Channels:        []channelJSON{},
					}
					for _, ch := range app.Channels {
						// 只报参数名，绝不报参数值
						names := make([]string, 0, len(ch.Params))
						for _, p := range ch.Params {
							names = append(names, p.Name)
						}
						item.Channels = append(item.Channels, channelJSON{
							ID: ch.Name, Enabled: ch.Enabled, Params: names,
						})
					}
					out.Apps = append(out.Apps, item)
				}
				return output.JSON(out)
			}

			if len(apps) == 0 {
				output.Line("还没有配置任何应用。用 `easy-publisher app add --id <包名> --name <名称>` 添加。")
				return nil
			}
			rows := make([][]string, 0, len(apps))
			for _, app := range apps {
				names := make([]string, 0, len(app.Channels))
				for _, ch := range app.EnabledChannels() {
					names = append(names, ch.Name)
				}
				enabled := "(无)"
				if len(names) > 0 {
					enabled = strings.Join(names, ", ")
				}
				rows = append(rows, []string{app.ApplicationID, app.Name, enabled})
			}
			output.Table([]string{"包名", "名称", "已启用渠道"}, rows)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	return cmd
}

func newAppAddCmd() *cobra.Command {
	var (
		id              string
		name            string
		label           string
		channels        []string
		multiChannelApk bool
		jsonOut         bool
	)
	cmd := &cobra.Command{
		Use:   "add",
		Short: "添加一个应用",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(id)
			if err != nil {
				return err
			}
			store := config.NewStore("")
			existing, found, err := store.Get(applicationID)
			if err != nil {
				return err
			}

			targets := channel.IDs()
			if len(channels) > 0 {
				targets = make([]string, 0, len(channels))
				for _, raw := range channels {
					ch, err := channel.Require(raw)
					if err != nil {
						return err
					}
					targets = append(targets, ch.ID())
				}
			}

			cfg := config.AppConfig{
				ApplicationID:   applicationID,
				Channels:        make([]config.ChannelConfig, 0, len(targets)),
				MultiChannelApk: multiChannelApk,
			}
			switch {
			case found:
				cfg.Name = existing.Name
				cfg.CreateTime = existing.CreateTime
				cfg.MultiChannelApk = multiChannelApk || existing.MultiChannelApk
				cfg.ExpectedLabel = existing.ExpectedLabel
			default:
				cfg.Name = applicationID
				cfg.CreateTime = time.Now().UnixMilli()
			}
			if name != "" {
				cfg.Name = name
			}
			if label != "" {
				cfg.ExpectedLabel = label
			}

			for _, channelID := range targets {
				// 保留已填写的参数，避免重复执行 add 把凭据清掉
				if found {
					if prev, ok := existing.Channel(channelID); ok {
						prev.Enabled = true
						cfg.Channels = append(cfg.Channels, prev)
						continue
					}
				}
				cfg.Channels = append(cfg.Channels, config.ChannelConfig{
					Name: channelID, Enabled: true,
				})
			}

			if err := store.Save(cfg); err != nil {
				return err
			}

			if jsonOut {
				return output.JSON(map[string]any{"ok": true, "applicationId": applicationID})
			}
			verb := "已添加"
			if found {
				verb = "已更新"
			}
			output.Line("%s 应用 %s，启用渠道：%s", verb, applicationID, strings.Join(targets, ", "))
			output.Line("下一步：为每个渠道填写凭据")
			for _, channelID := range targets {
				ch, _ := channel.Find(channelID)
				for _, p := range ch.Params() {
					output.Line("  easy-publisher channel set --app %s --channel %s --key %s --value <%s>",
						applicationID, channelID, p.Name, p.Description)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "包名，如 com.example.app（必填）")
	cmd.Flags().StringVar(&name, "name", "", "应用名称，默认用包名")
	cmd.Flags().StringSliceVar(&channels, "channels",
		nil, "启用的渠道，逗号分隔。可用："+strings.Join(channel.IDs(), ", "))
	cmd.Flags().BoolVar(&multiChannelApk, "multi-channel-apk", false,
		"每个渠道使用独立的渠道包（按文件名中的渠道标识匹配）")
	cmd.Flags().StringVar(&label, "label", "",
		"商店页展示的应用名，checklist 用它比对 APK 内的 android:label")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}

// newAppSetCmd 更新应用配置里与发布检查相关的字段。
//
// 与 add 分开：add 处理「有哪些渠道」，set 处理「这些元信息是什么」，
// 混在一起会让「只想补一个应用名」变成一次容易误清渠道的操作。
func newAppSetCmd() *cobra.Command {
	var (
		id      string
		label   string
		name    string
		jsonOut bool
	)
	cmd := &cobra.Command{
		Use:   "set",
		Short: "修改应用信息（不改动渠道与凭据）",
		Long: "修改应用信息。只动你指定的字段，渠道配置与凭据原样保留。\n\n" +
			"--label 用于 checklist 的应用名一致性检查：它应当是商店页展示的名字，" +
			"checklist 会拿它与 APK 内的 android:label 比对。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(id)
			if err != nil {
				return err
			}
			if label == "" && name == "" {
				return eperr.ConfigurationError("至少要指定 --label 或 --name 之一")
			}

			store := config.NewStore("")
			cfg, found, err := store.Get(applicationID)
			if err != nil {
				return err
			}
			if !found {
				return eperr.ConfigurationError(
					"没有找到应用 %s，先用 `app add --id %s` 添加", applicationID, applicationID)
			}

			if label != "" {
				cfg.ExpectedLabel = label
			}
			if name != "" {
				cfg.Name = name
			}
			if err := store.Save(cfg); err != nil {
				return err
			}

			if jsonOut {
				return output.JSON(map[string]any{
					"ok": true, "applicationId": applicationID,
					"name": cfg.Name, "expectedLabel": cfg.ExpectedLabel,
				})
			}
			output.Line("已更新 %s：应用名=%s，商店页名称=%s",
				applicationID, cfg.Name, cfg.ExpectedLabel)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "app", "", "包名（必填）")
	cmd.Flags().StringVar(&label, "label", "", "商店页展示的应用名")
	cmd.Flags().StringVar(&name, "name", "", "配置里的应用名（仅本地展示用）")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	_ = cmd.MarkFlagRequired("app")
	return cmd
}

func newAppRemoveCmd() *cobra.Command {
	var id string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "删除应用配置",
		Long:  "删除应用配置。凭据会被覆写后真正删除，不保留备份。",
		RunE: func(cmd *cobra.Command, _ []string) error {
			applicationID, err := artifact.ValidateApplicationID(id)
			if err != nil {
				return err
			}
			removed, err := config.NewStore("").Remove(applicationID)
			if err != nil {
				return err
			}
			if jsonOut {
				return output.JSON(map[string]any{"ok": true, "removed": removed})
			}
			if removed {
				output.Line("已删除 %s 的配置及其凭据", applicationID)
			} else {
				output.Line("没有找到 %s 的配置", applicationID)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "包名（必填）")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "输出 JSON")
	_ = cmd.MarkFlagRequired("id")
	return cmd
}
