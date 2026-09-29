// Package builtin 把所有内置渠道注册到注册表。
//
// 单独成包是为了打破依赖环：channel 包不能引用具体渠道实现
// （那会让每个渠道都反向依赖 channel 的接口定义）。
// 由本包在启动时统一注册，调用方 import 一次即可。
package builtin

import (
	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel/harmony"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel/honor"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel/huawei"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel/mi"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel/oppo"
	"github.com/fangxiangCN/easy-publisher/go/internal/channel/vivo"
)

// Channels 是全部内置渠道的构造函数，按注册顺序排列。
//
// 顺序与 Kotlin 版一致（华为、小米、OPPO、vivo、荣耀、鸿蒙）——
// 能力矩阵表的输出顺序依赖它。
var Channels = []func() channel.Channel{
	func() channel.Channel { return huawei.New() },
	func() channel.Channel { return mi.New() },
	func() channel.Channel { return oppo.New() },
	func() channel.Channel { return vivo.New() },
	func() channel.Channel { return honor.New() },
	func() channel.Channel { return harmony.New() },
}

// Register 把全部内置渠道注册到注册表。重复调用是幂等的。
func Register() {
	for _, newChannel := range Channels {
		channel.Register(newChannel())
	}
}
