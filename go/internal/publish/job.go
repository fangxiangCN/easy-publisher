package publish

import (
	"errors"
	"fmt"

	"github.com/fangxiangCN/easy-publisher/go/internal/channel"
	"github.com/fangxiangCN/easy-publisher/go/internal/eperr"
)

// StageKind 是单个渠道的提交阶段。
type StageKind int

const (
	StageWaiting StageKind = iota
	StageWorking
	StageUploading
	StageSucceeded
	StageFailed
	StageCancelled
)

func (k StageKind) String() string {
	switch k {
	case StageWaiting:
		return "Waiting"
	case StageWorking:
		return "Working"
	case StageUploading:
		return "Uploading"
	case StageSucceeded:
		return "Succeeded"
	case StageFailed:
		return "Failed"
	default:
		return "Cancelled"
	}
}

// Stage 是一个渠道的当前状态。
//
// Go 没有 sealed class，因此用「Kind + 各阶段各自的字段」而不是接口多实现 ——
// 这样 JSON 序列化与调用方判断都更直接，代价是部分字段在某些 Kind 下无意义。
type Stage struct {
	Kind StageKind

	// Action 仅 StageWorking 有意义，如「获取 token」「绑定文件」
	Action string
	// Fraction 仅 StageUploading 有意义，取值 [0,1]
	Fraction float64

	// Reached 仅 StageSucceeded 有意义：实际到达的阶段。
	//
	// 停在草稿态时不能显示「已提交」，那会让使用者以为版本已经送审。
	Reached channel.ReleaseStage

	// 以下仅 StageFailed 有意义
	ErrKind   eperr.ErrorKind
	Code      string
	Message   string
	Retryable bool
	// Phase 解释「为什么不可重试」：越过送审点后无法确定服务端是否已受理，
	// 重试可能造成重复版本
	Phase eperr.FailurePhase
}

// Label 是表格用的简短结论。
func (s Stage) Label() string {
	switch s.Kind {
	case StageWaiting:
		return "等待中"
	case StageWorking:
		return s.Action
	case StageUploading:
		return fmt.Sprintf("上传中 %d%%", int(s.Fraction*100))
	case StageSucceeded:
		switch s.Reached {
		case channel.StageUploadArtifact:
			return "已上传安装包（未创建版本）"
		case channel.StageCreateDraft:
			return "草稿已就绪（未送审）"
		default:
			return "已提交审核"
		}
	case StageCancelled:
		return "已取消"
	default:
		return s.Summary()
	}
}

// Summary 是失败时的简短结论。
//
// Message 在越过送审点时会带上「先到后台确认」的长提示，塞进表格会把列宽撑爆，
// 所以表格只显示结论，完整原因另走 stderr 或 JSON 输出。
func (s Stage) Summary() string {
	switch {
	case s.Phase == eperr.PhaseAtOrAfterSubmission:
		return fmt.Sprintf("失败（%s，已送审需人工确认）", s.ErrKind.Label())
	case s.Code != "":
		return fmt.Sprintf("失败（%s code=%s）", s.ErrKind.Label(), s.Code)
	default:
		return fmt.Sprintf("失败（%s）", s.ErrKind.Label())
	}
}

// Terminal 报告是否已到终态。
func (s Stage) Terminal() bool {
	return s.Kind == StageSucceeded || s.Kind == StageFailed || s.Kind == StageCancelled
}

// StageOfError 把错误转成失败状态。
func StageOfError(err error) Stage {
	var pe *eperr.Error
	if !errors.As(err, &pe) {
		pe = eperr.FromError("", err)
	}
	return Stage{
		Kind:      StageFailed,
		ErrKind:   pe.Kind,
		Code:      pe.Code,
		Message:   pe.Describe(),
		Retryable: pe.Retryable(),
		Phase:     pe.Phase,
	}
}

// ChannelProgress 是一个渠道的进度。
type ChannelProgress struct {
	ChannelID   string
	DisplayName string
	Stage       Stage
}

// JobState 是整个任务的状态。
type JobState int

const (
	JobRunning JobState = iota
	JobSucceeded
	JobPartiallyFailed
	JobFailed
	JobCancelled
)

func (s JobState) String() string {
	switch s {
	case JobSucceeded:
		return "Succeeded"
	case JobPartiallyFailed:
		return "PartiallyFailed"
	case JobFailed:
		return "Failed"
	case JobCancelled:
		return "Cancelled"
	default:
		return "Running"
	}
}

// Job 是一次发布任务的快照。
//
// 上传一个大包动辄数分钟到数十分钟，而 MCP 的工具调用是一次请求一次响应，
// 不能让 agent 阻塞等待。因此 Submit 立即返回 id，调用方轮询快照。
type Job struct {
	ID            string
	ApplicationID string
	ArtifactPath  string
	VersionCode   int64
	VersionName   string
	Channels      []ChannelProgress

	// RequestedStage 是调用方显式请求的停留点，nil 表示「各渠道走到各自能到的
	// 最远阶段」。此时不同渠道的目标阶段可能不同（例如同时发华为与鸿蒙），
	// 所以无法用一个任务级的值概括。
	RequestedStage *channel.ReleaseStage

	StartedAt  int64
	FinishedAt int64 // 0 表示尚未结束
}

// State 从各渠道的阶段推导任务整体状态。
func (j Job) State() JobState {
	if len(j.Channels) == 0 {
		return JobRunning
	}
	var succeeded, cancelled int
	for _, c := range j.Channels {
		if !c.Stage.Terminal() {
			return JobRunning
		}
		switch c.Stage.Kind {
		case StageSucceeded:
			succeeded++
		case StageCancelled:
			cancelled++
		}
	}
	switch {
	case succeeded == len(j.Channels):
		return JobSucceeded
	case cancelled == len(j.Channels):
		return JobCancelled
	case succeeded > 0:
		// 部分成功也要能被识别出来，否则 CI 会当成全部失败而重试已经成功的渠道
		return JobPartiallyFailed
	default:
		return JobFailed
	}
}

// Done 报告任务是否已结束。
func (j Job) Done() bool { return j.State() != JobRunning }

// Succeeded 返回成功的渠道 id。
func (j Job) Succeeded() []string {
	var out []string
	for _, c := range j.Channels {
		if c.Stage.Kind == StageSucceeded {
			out = append(out, c.ChannelID)
		}
	}
	return out
}

// Failed 返回失败的渠道。
func (j Job) Failed() []ChannelProgress {
	var out []ChannelProgress
	for _, c := range j.Channels {
		if c.Stage.Kind == StageFailed {
			out = append(out, c)
		}
	}
	return out
}
