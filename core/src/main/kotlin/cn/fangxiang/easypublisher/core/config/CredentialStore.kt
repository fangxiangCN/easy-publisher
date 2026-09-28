package cn.fangxiang.easypublisher.core.config

import cn.fangxiang.easypublisher.core.PublishError
import cn.fangxiang.easypublisher.core.log.AppLogger

/**
 * 渠道凭据的来源。
 *
 * 凭据是**程序自己读**的，不是调用方传进来的 —— CLI 与 MCP 的对外接口都不接受
 * secret 参数。这样密钥不会进入 shell 历史、CI 日志，也不会进入 AI 模型的对话上下文。
 *
 * 两种来源见 [ConfigCredentialStore]（配置文件）与 [EnvCredentialStore]（环境变量），
 * 优先级由 [LayeredCredentialStore] 组合决定。
 */
interface CredentialStore {

    /**
     * 读取一个渠道参数。
     *
     * @return 未配置或值为空时返回 null。空字符串一律视为「未配置」，
     *   否则一个 `--value ""` 就能把必填校验绕过去，直到请求发出才失败。
     */
    fun get(applicationId: String, channelId: String, paramName: String): String?
}

/**
 * 从已加载的应用配置里读凭据。
 *
 * [applicationId] 必须与持有的配置一致：配置对象由调用方传递，一旦错配就会用
 * A 应用的密钥去发布 B 应用的包 —— 这正是原项目进程级单例持有可变
 * clientId/clientSecret 的后果（见 Channel.kt 的说明），此处宁可大声失败。
 */
class ConfigCredentialStore(private val config: AppConfig) : CredentialStore {

    override fun get(applicationId: String, channelId: String, paramName: String): String? {
        requireSameApp(applicationId, config)
        return config.channel(channelId)?.value(paramName)
    }
}

/**
 * 从环境变量读凭据，格式 `EP_<渠道>_<参数>`，例如 `EP_HUAWEI_CLIENT_SECRET`。
 *
 * CI 场景可以完全不落盘，凭据只存在于进程环境里。
 *
 * 环境变量是**进程级**的，不区分应用 —— 若同一个环境里发布多个应用、而它们同名参数
 * 取值不同，环境变量会覆盖所有应用。多应用场景请用配置文件。
 *
 * [environment] 可注入，便于测试。
 */
class EnvCredentialStore(
    private val environment: Map<String, String> = System.getenv(),
) : CredentialStore {

    override fun get(applicationId: String, channelId: String, paramName: String): String? =
        environment[envName(channelId, paramName)]?.takeIf { it.isNotBlank() }

    companion object {

        private const val PREFIX = "EP"

        private val NON_ALNUM = Regex("[^A-Za-z0-9]+")
        private val CAMEL_BOUNDARY = Regex("([a-z0-9])([A-Z])")
        private val UNDERSCORE_RUN = Regex("_+")

        /**
         * 参数名到环境变量名的映射。
         *
         * `client_secret` → `EP_HUAWEI_CLIENT_SECRET`；
         * 驼峰也切成下划线，`publicKey` → `EP_MI_PUBLIC_KEY`，
         * 免得用户要猜是 `PUBLICKEY` 还是 `PUBLIC_KEY`。
         *
         * `channel list` 会逐个参数打印这个名字，不必自己推导。
         */
        fun envName(channelId: String, paramName: String): String =
            "${PREFIX}_${normalize(channelId)}_${normalize(paramName)}"

        private fun normalize(raw: String): String =
            raw.replace(NON_ALNUM, "_")
                .replace(CAMEL_BOUNDARY, "$1_$2")
                .uppercase()
                .let { UNDERSCORE_RUN.replace(it, "_") }
                .trim('_')
    }
}

/**
 * 组合两种来源，**环境变量优先**。
 *
 * 顺序是刻意的：CI 里临时覆盖某个参数（例如换用测试环境的 client_secret）
 * 不必改动磁盘上的配置文件。反过来若配置文件优先，环境变量形同虚设，
 * 而使用者会以为已经生效 —— 这种「以为改了其实没改」比缺参数更难排查，
 * 因为它表现为拿旧凭据静默失败。
 *
 * 命中的来源记一条 debug 日志（**只记参数名与来源，不记值**），
 * 用于排查「为什么用的还是旧凭据」。
 *
 * [applicationId] 同样要与配置一致，理由见 [ConfigCredentialStore]。
 */
class LayeredCredentialStore(
    private val config: AppConfig,
    private val environment: EnvCredentialStore = EnvCredentialStore(),
) : CredentialStore {

    private val file = ConfigCredentialStore(config)

    override fun get(applicationId: String, channelId: String, paramName: String): String? {
        requireSameApp(applicationId, config)

        environment.get(applicationId, channelId, paramName)?.let {
            AppLogger.debug(TAG, "凭据 $channelId/$paramName 取自环境变量")
            return it
        }
        return file.get(applicationId, channelId, paramName)?.also {
            AppLogger.debug(TAG, "凭据 $channelId/$paramName 取自配置文件")
        }
    }

    private companion object {
        const val TAG = "credential"
    }
}

/**
 * 校验配置对象的归属。
 *
 * 不一致说明调用方传错了配置，继续执行会用错应用的密钥 —— 宁可立刻失败。
 * 消息里只出现包名，不含任何凭据值。
 */
private fun requireSameApp(applicationId: String, config: AppConfig) {
    if (applicationId != config.applicationId) {
        throw PublishError.configuration(
            "凭据来源与目标应用不一致：请求 $applicationId，持有 ${config.applicationId}。" +
                "这通常意味着编排层传错了配置对象"
        )
    }
}
