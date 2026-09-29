package builtin

import (
	"strings"
	"testing"

	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
)

func TestRegisterAll(t *testing.T) {
	channel.Reset()
	Register()
	got := channel.IDs()
	want := []string{"huawei", "mi", "oppo", "vivo", "honor", "harmony"}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("IDs = %v, 期望 %v", got, want)
	}
	// 重复注册应当幂等
	Register()
	if n := len(channel.All()); n != 6 {
		t.Errorf("重复注册后渠道数 = %d, 期望 6", n)
	}
}
