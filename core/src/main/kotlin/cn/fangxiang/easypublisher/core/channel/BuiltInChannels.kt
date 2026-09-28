package cn.fangxiang.easypublisher.core.channel

import cn.fangxiang.easypublisher.core.channel.honor.HonorChannel
import cn.fangxiang.easypublisher.core.channel.huawei.HuaweiChannel
import cn.fangxiang.easypublisher.core.channel.mi.MiChannel
import cn.fangxiang.easypublisher.core.channel.oppo.OppoChannel
import cn.fangxiang.easypublisher.core.channel.vivo.VivoChannel

/**
 * 内置渠道清单。
 *
 * 这些实例被 [ChannelRegistry] 全局共享，前提是渠道实现无状态 ——
 * 凭据与进度回调都走方法入参（见 [UploadRequest]）。
 */
internal fun builtIn(): List<Channel> = listOf(
    HuaweiChannel(),
    MiChannel(),
    OppoChannel(),
    VivoChannel(),
    HonorChannel(),
)
