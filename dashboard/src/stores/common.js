import { defineStore } from "pinia";
import { logApi, pluginApi, statsApi, systemConfigApi } from "@/api/v1";
import { fetchWithAuth } from "@/api/http";
import { getToken } from "@/utils/token";

export const useCommonStore = defineStore("common", {
  state: () => ({
    // @ts-ignore
    eventSource: null,
    log_cache: [],
    sse_connected: false,
    sseRetryCount: 0,

    log_cache_max_len: 1000,
    startTime: -1,
    astrbotVersion: "",
    dashboardVersion: "",
    pythonVersion: "",
    goVersion: "",
    // 设置里的 github_proxy（system-config 持久化值）：安装插件/切换版本的
    // GitHub 加速弹窗默认选中项，用户在弹窗里的单次选择可临时覆盖。
    githubProxyConfig: "",

    pluginMarketData: [],
    pluginMarketDataBySource: {},
  }),
  actions: {
    async createEventSource() {
      if (this.eventSource) {
        return;
      }
      const controller = new AbortController();
      const { signal } = controller;

      // 注意：这里如果之前改过 Polyfill 的话，可能需要保持原样
      // 如果是用 fetch 的话，这里是支持 Authorization Header 的
      const headers = {
        "Content-Type": "multipart/form-data",
        Authorization: "Bearer " + getToken(),
      };

      fetchWithAuth(logApi.liveUrl(), {
        method: "GET",
        headers,
        signal,
        cache: "no-cache",
      })
        .then((response) => {
          if (!response.ok) {
            throw new Error(`SSE connection failed: ${response.status}`);
          }
          console.log("SSE stream opened");
          this.sse_connected = true;
          this.sseRetryCount = 0;

          const reader = response.body.getReader();
          const decoder = new TextDecoder();
          let bufferedText = "";

          const processStream = ({ done, value }) => {
            if (done) {
              this.sse_connected = false;
              // 服务端主动关闭也走指数退避，避免固定 2s 打雷式重连
              const delay = Math.min(30000, 1000 * 2 ** this.sseRetryCount++);
              setTimeout(() => {
                this.eventSource = null;
                this.createEventSource();
              }, delay);
              return;
            }

            // Accumulate partial chunks; SSE data may split JSON across reads.
            const text = decoder.decode(value, { stream: true });
            bufferedText += text;

            // Split completed events; keep the trailing partial in buffer.
            const segments = bufferedText.split("\n\n");
            bufferedText = segments.pop() || "";

            segments.forEach((segment) => {
              const line = segment.trim();
              if (!line.startsWith("data: ")) {
                return;
              }

              const logLine = line.replace("data: ", "").trim();
              if (!logLine) {
                return;
              }

              try {
                const logObject = JSON.parse(logLine);

                // 修复：兼容 HTTP 环境的 UUID 生成
                if (!logObject.uuid) {
                  if (
                    typeof crypto !== "undefined" &&
                    typeof crypto.randomUUID === "function"
                  ) {
                    logObject.uuid = crypto.randomUUID();
                  } else {
                    // 手动生成 UUID v4
                    logObject.uuid =
                      "xxxxxxxx-xxxx-4xxx-yxxx-xxxxxxxxxxxx".replace(
                        /[xy]/g,
                        function (c) {
                          var r = (Math.random() * 16) | 0,
                            v = c == "x" ? r : (r & 0x3) | 0x8;
                          return v.toString(16);
                        },
                      );
                  }
                }

                this.log_cache.push(logObject);
                // Limit log cache size
                if (this.log_cache.length > this.log_cache_max_len) {
                  this.log_cache.splice(
                    0,
                    this.log_cache.length - this.log_cache_max_len,
                  );
                }
              } catch (err) {
                console.warn(
                  "Failed to parse SSE log line, skipping:",
                  err,
                  logLine,
                );
              }
            });

            return reader.read().then(processStream);
          };

          reader.read().then(processStream);
        })
        .catch((error) => {
          console.error("SSE error:", error);
          // Attempt to reconnect with exponential backoff
          const delay = Math.min(30000, 1000 * 2 ** this.sseRetryCount++);
          this.log_cache.push({
            type: "log",
            level: "ERROR",
            time: Date.now() / 1000,
            data: `SSE Connection failed, retrying in ${Math.round(delay / 1000)} seconds...`,
            uuid: "error-" + Date.now(),
          });
          setTimeout(() => {
            this.eventSource = null;
            this.createEventSource();
          }, delay);
        });

      // Store controller to allow closing the connection
      this.eventSource = controller;
    },
    closeEventSource() {
      if (this.eventSource) {
        this.eventSource.abort();
        this.eventSource = null;
      }
      this.sse_connected = false;
    },
    getLogCache() {
      return this.log_cache;
    },
    async fetchStartTime() {
      const res = await statsApi.startTime();
      this.startTime = res.data.data.start_time;
      return this.startTime;
    },
    setAstrBotVersion(version, dashboardVersion = "", pythonVersion = "", goVersion = "") {
      this.astrbotVersion = String(version || "").replace(/^v/i, "");
      this.dashboardVersion = String(dashboardVersion || "");
      this.pythonVersion = String(pythonVersion || "").replace(/^v/i, "");
      this.goVersion = String(goVersion || "").replace(/^go/i, "");
    },
    // 拉取设置里的 github_proxy（缓存到 githubProxyConfig），供代理选择
    // 弹窗作为默认选中项。失败静默（弹窗回退 localStorage/空）。
    async fetchGithubProxyConfig(force = false) {
      if (!force && this.githubProxyConfig) {
        return this.githubProxyConfig;
      }
      try {
        const res = await systemConfigApi.get();
        const cfg = res.data?.data?.config || {};
        this.githubProxyConfig = String(cfg.github_proxy || "").trim();
      } catch {
        // keep current value
      }
      return this.githubProxyConfig;
    },
    async fetchAstrBotVersion(force = false) {
      // pythonVersion 为空说明尚未拉到 /stats/version（含 python_version 字
      // 段），此时即使 astrbotVersion 有缓存也要强制拉取，否则 Python 插件
      // 的 astrbot_version 校验永远拿不到 4.x 宿主版本。
      if (!force && this.astrbotVersion && this.pythonVersion) {
        return this.astrbotVersion;
      }
      const res = await statsApi.version();
      const data = res.data?.data || {};
      this.setAstrBotVersion(
        data.version,
        data.dashboard_version,
        data.python_version,
        data.go_version,
      );
      return this.astrbotVersion;
    },
    getStartTime() {
      if (this.startTime !== -1) {
        return this.startTime;
      }
      this.fetchStartTime().catch(() => {});
      return this.startTime;
    },
    async getPluginCollections(force = false, customSource = null) {
      // 获取插件市场数据
      const sourceKey = String(customSource || "")
        .trim()
        .replace(/\/+$/, "");
      if (!force) {
        if (!sourceKey && this.pluginMarketData.length > 0) {
          return Promise.resolve(this.pluginMarketData);
        }
        if (
          sourceKey &&
          Array.isArray(this.pluginMarketDataBySource[sourceKey])
        ) {
          return Promise.resolve(this.pluginMarketDataBySource[sourceKey]);
        }
      }

      return pluginApi
        .market({
          force_refresh: force || undefined,
          custom_registry: sourceKey || undefined,
        })
        .then((res) => {
          let data = [];
          if (res.data.data && typeof res.data.data === "object") {
            for (let key in res.data.data) {
              if (key === "$meta") {
                continue;
              }

              const pluginData = res.data.data[key];
              const fallbackPluginName = String(key || "").includes("/")
                ? ""
                : String(key || "").trim();
              const pluginAuthor = String(pluginData?.author || "").trim();
              const pluginName =
                String(pluginData?.name || "").trim() || fallbackPluginName;
              const displayPluginName = pluginName || key;
              const marketPluginId =
                String(pluginData?.market_plugin_id || "").trim() ||
                (pluginAuthor && pluginName
                  ? `${pluginAuthor}/${pluginName}`
                  : "");
              const parsedDownloadCount = Number(pluginData?.download_count);
              const downloadCount =
                pluginData?.download_count === undefined ||
                pluginData?.download_count === null ||
                pluginData?.download_count === "" ||
                !Number.isFinite(parsedDownloadCount)
                  ? undefined
                  : Math.max(0, Math.trunc(parsedDownloadCount));

              data.push({
                ...pluginData,
                name: displayPluginName, // 优先使用插件数据中的name字段，否则使用键名
                market_plugin_id: marketPluginId,
                desc: pluginData.desc,
                short_desc: pluginData?.short_desc ? pluginData.short_desc : "",
                author: pluginData.author,
                repo: pluginData.repo,
                installed: false,
                version: pluginData?.version ? pluginData.version : "未知",
                social_link: pluginData?.social_link,
                tags: pluginData?.tags ? pluginData.tags : [],
                logo: pluginData?.logo ? pluginData.logo : "",
                pinned: pluginData?.pinned ? pluginData.pinned : false,
                stars: pluginData?.stars ? pluginData.stars : 0,
                download_count: downloadCount,
                updated_at: pluginData?.updated_at ? pluginData.updated_at : "",
                download_url: pluginData?.download_url
                  ? pluginData.download_url
                  : "",
                display_name: pluginData?.display_name
                  ? pluginData.display_name
                  : "",
                i18n:
                  pluginData?.i18n && typeof pluginData.i18n === "object"
                    ? pluginData.i18n
                    : {},
                astrbot_version: pluginData?.astrbot_version
                  ? pluginData.astrbot_version
                  : "",
                category: pluginData?.category ? pluginData.category : "",
                support_platforms: Array.isArray(pluginData?.support_platforms)
                  ? pluginData.support_platforms
                  : Array.isArray(pluginData?.support_platform)
                  ? pluginData.support_platform
                  : Array.isArray(pluginData?.platform)
                  ? pluginData.platform
                  : [],
              });
            }
          }

          if (sourceKey) {
            this.pluginMarketDataBySource[sourceKey] = data;
          } else {
            this.pluginMarketData = data;
          }
          return data;
        })
        .catch((err) => {
          return Promise.reject(err);
        });
    },
  },
});
