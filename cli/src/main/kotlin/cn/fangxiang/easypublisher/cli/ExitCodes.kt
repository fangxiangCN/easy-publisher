package cn.fangxiang.easypublisher.cli

import cn.fangxiang.easypublisher.core.ErrorKind

/**
 * 退出码。脚本与 CI 依据此值决定后续动作，因此语义必须稳定。
 */
object ExitCodes {
    const val SUCCESS = 0

    /** 参数错误、配置缺失 */
    const val USAGE = 1

    /** 凭据缺失或被渠道拒绝 */
    const val CREDENTIAL = 2

    /** 渠道返回业务错误 */
    const val CHANNEL_REJECTED = 3

    /** 网络失败或超时，通常可重试 */
    const val NETWORK = 4

    /** 不满足发布前置条件，如版本号不大于线上版本 */
    const val PRECONDITION = 5

    /** 本地文件问题 */
    const val LOCAL_FILE = 6

    /** 渠道响应无法解析，接口可能已变更 */
    const val PROTOCOL = 7

    fun of(kind: ErrorKind): Int = when (kind) {
        ErrorKind.Configuration -> USAGE
        ErrorKind.Credential -> CREDENTIAL
        ErrorKind.LocalFile -> LOCAL_FILE
        ErrorKind.Network -> NETWORK
        ErrorKind.ChannelRejected -> CHANNEL_REJECTED
        ErrorKind.ProtocolMismatch -> PROTOCOL
        ErrorKind.Precondition -> PRECONDITION
        ErrorKind.Unknown -> 1
    }
}
