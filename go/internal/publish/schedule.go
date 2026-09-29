package publish

import (
	"strings"
	"time"

	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// ParseOnlineTime 解析定时上线时间。
//
// 格式与 Kotlin 版一致：yyyy-MM-dd HH:mm:ss，按本地时区解释。
//
// 注意华为与荣耀的接口要求的是另一个格式（yyyy-MM-dd'T'HH:mm:ssZZ，且时区偏移
// 不带冒号），那是渠道内部的转换，与本函数的输入格式是两回事。
func ParseOnlineTime(text string) (int64, error) {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(text), time.Local)
	if err != nil {
		return 0, eperr.ConfigurationError(
			"定时上线时间格式不正确：%s，应为 yyyy-MM-dd HH:mm:ss", text)
	}
	if t.Before(time.Now()) {
		return 0, eperr.ConfigurationError("定时上线时间不能早于当前时间：%s", text)
	}
	return t.UnixMilli(), nil
}
