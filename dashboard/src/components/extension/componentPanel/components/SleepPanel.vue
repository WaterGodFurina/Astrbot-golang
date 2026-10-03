<script setup lang="ts">
/**
 * 休眠策略面板 - 组件管理页第三个视图（与"指令/函数工具"按钮同列）。
 * 每插件的"允许休眠"开关 + 独立闲置分钟数（0 = 回退全局默认）+ 唤醒方式
 * （仅插件唤醒 / 过滤器钩子+指令唤醒）+ 过滤器/钩子风险提示。顶部"新装
 * 插件默认休眠时间"仅作为新装插件的默认阈值，不覆盖已单独配置的插件
 * （也不控制所有插件的开关）。
 */
import { computed, onMounted, ref } from "vue";
import { pluginApi } from "@/api/v1";
import { fetchWithAuth } from "@/api/http";
import { useModuleI18n } from "@/i18n/composables";

defineOptions({ name: "SleepPanel" });

const props = withDefaults(defineProps<{ active?: boolean }>(), {
  active: true,
});

const { tm } = useModuleI18n("features/command");

interface SleepPluginItem {
  id: string;
  displayName: string;
  language: string;
  enabled: boolean;
  allowSleep: boolean;
  idleUnloadMinutes: number;
  idleWakeMode: string;
  hasFilter: boolean;
  hasHook: boolean;
  activeEventListener: boolean;
  version: string;
  runtime: string;
  nativeConfirmed: boolean;
  runtimeCurrent: string;
  healthState: string;
  healthReason: string;
}

// 休眠唤醒方式选项：command_only = 插件指令+工具唤醒（默认；钩子/被动事件不唤醒）；
// hook_and_command = 插件指令+工具+过滤器唤醒。
const WAKE_COMMAND_ONLY = "command_only";
const WAKE_HOOK_AND_COMMAND = "hook_and_command";

const wakeModeItems = computed(() => [
  { value: WAKE_HOOK_AND_COMMAND, title: tm("sleep.wakeHookAndCommand") },
  { value: WAKE_COMMAND_ONLY, title: tm("sleep.wakeCommandOnly") },
]);

// Python 首选运行方式选项：shared（共享进程，推荐）/ grpc（独立进程）。
// isolated 由系统在故障时自动隔离，不作为用户选项。
const pythonRuntimeItems = computed(() => [
  { value: PY_RUNTIME_SHARED, title: tm("sleep.runtimePythonShared") },
  { value: PY_RUNTIME_GRPC, title: tm("sleep.runtimePythonGrpc") },
]);

// 插件语言从 id 后缀（_go/_python）推断，优先用后端 language 字段。
const languageOf = (p: Record<string, unknown>) => {
  const lang = String(p.language || "").toLowerCase();
  if (lang === "python") return "python";
  if (lang === "go" || lang === "golang") return "golang";
  const id = String(p.id || p.name || "");
  if (/_python$/i.test(id)) return "python";
  if (/_go$/i.test(id)) return "golang";
  return "";
};

// 运行方式：Go 为 grpc/native；Python 为 preferred（shared 共享进程 /
// grpc 独立进程）。isolated 不是用户选项——它是 Watchdog 在故障时自动隔离
// 后派生的状态（preferred 保持 shared，current 变为 python-grpc）。
const PY_RUNTIME_SHARED = "shared";
const PY_RUNTIME_GRPC = "grpc";
const normalizeRuntime = (raw: string, lang: string) => {
  const v = String(raw || "").toLowerCase();
  if (lang === "python") {
    if (v === PY_RUNTIME_SHARED) return PY_RUNTIME_SHARED;
    if (v === "python-isolated" || v === "python-grpc" || v === "isolated") {
      return PY_RUNTIME_GRPC;
    }
    return v === PY_RUNTIME_GRPC ? PY_RUNTIME_GRPC : PY_RUNTIME_SHARED;
  }
  return v === "native" ? "native" : "grpc";
};

const loading = ref(false);
const saving = ref(false);
const plugins = ref<SleepPluginItem[]>([]);
const snackbar = ref<{ show: boolean; message: string; color: string }>({
  show: false,
  message: "",
  color: "success",
});

const toast = (message: string, color = "success") => {
  snackbar.value.message = message;
  snackbar.value.color = color;
  snackbar.value.show = true;
};

const fetchData = async () => {
  loading.value = true;
  try {
    const listRes = await pluginApi.list();
    if (listRes.data.status === "ok") {
      const items = (listRes.data.data || []) as Array<Record<string, unknown>>;
      plugins.value = items
        .filter((p) => p.enabled)
        .map((p) => ({
          id: String(p.id || p.name || ""),
          displayName: String(p.display_name || p.name || p.id || ""),
          language: languageOf(p),
          enabled: Boolean(p.enabled),
          allowSleep: Boolean(p.idle_unload),
          idleUnloadMinutes: Number(p.idle_unload_minutes || 0),
          idleWakeMode: String(p.idle_wake_mode || WAKE_COMMAND_ONLY),
          hasFilter: Boolean(p.has_filter),
          hasHook: Boolean(p.has_hook),
          activeEventListener: Boolean(p.active_event_listener),
          version: String(p.version || ""),
          runtime: normalizeRuntime(String(p.runtime_preferred || p.runtime || ""), languageOf(p)),
          runtimeCurrent: String(p.runtime_current || ""),
          healthState: String(p.health_state || ""),
          healthReason: String(p.health_reason || ""),
          nativeConfirmed: Boolean(p.native_confirmed),
        }));
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    loading.value = false;
  }
};

/** 开启休眠时不传阈值，由后端落 plugin.DefaultIdleUnloadMinutes（单一真源在后端）。 */
const togglePlugin = async (item: SleepPluginItem, allowSleep: boolean) => {
  if (saving.value || !item.id) return;
  saving.value = true;
  try {
    const res = await pluginApi.setIdleSleep(item.id, allowSleep);
    if (res.data.status === "ok") {
      item.allowSleep = allowSleep;
      const echo = (res.data as any)?.data?.idle_unload_minutes;
      if (allowSleep && typeof echo === "number") {
        item.idleUnloadMinutes = echo;
      }
      toast(tm("sleep.pluginSaved"));
    } else {
      toast(
        (res.data as any)?.message || tm("messages.operationFailed"),
        "error",
      );
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
  }
};

const savePluginMinutes = async (
  item: SleepPluginItem,
  raw: number | string | null,
) => {
  const minutes = Math.max(0, Number(raw) || 0);
  if (saving.value || !item.id) return;
  saving.value = true;
  try {
    const res = await pluginApi.setIdleSleep(item.id, item.allowSleep, minutes);
    if (res.data.status === "ok") {
      item.idleUnloadMinutes = minutes;
      toast(tm("sleep.pluginSaved"));
    } else {
      toast(
        (res.data as any)?.message || tm("messages.operationFailed"),
        "error",
      );
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
  }
};

const savePluginWakeMode = async (item: SleepPluginItem, mode: string) => {
  if (saving.value || !item.id) return;
  saving.value = true;
  try {
    const res = await pluginApi.setIdleSleep(
      item.id,
      item.allowSleep,
      item.idleUnloadMinutes,
      mode,
    );
    if (res.data.status === "ok") {
      item.idleWakeMode = mode;
      toast(tm("sleep.pluginSaved"));
    } else {
      toast(
        (res.data as any)?.message || tm("messages.operationFailed"),
        "error",
      );
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
  }
};

// ---- 运行方式（gRPC / Native，仅 Go 插件） ----
// Native 切换需用户先确认风险警告（后端强制），且重新构建 + 重启后生效。
const isGoPlugin = (item: SleepPluginItem) => item.language === "golang";
const isPythonPlugin = (item: SleepPluginItem) => item.language === "python";
const runtimeDialog = ref<{
  show: boolean;
  item: SleepPluginItem | null;
}>({ show: false, item: null });

const applyRuntime = async (item: SleepPluginItem, native: boolean) => {
  if (saving.value || !item.id) return;
  saving.value = true;
  try {
    const res = await pluginApi.setRuntime(item.id, native);
    if (res.data.status === "ok") {
      item.runtime = native ? "native" : "grpc";
      toast(tm("sleep.runtimeSaved"));
    } else {
      toast(
        (res.data as any)?.message || tm("messages.operationFailed"),
        "error",
      );
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
  }
};

// pythonRuntimeStatus 计算 Python 插件当前状态文案：正常显示实际部署方式；
// 被系统自动隔离时显示「已隔离 + 原因」（preferred 仍为用户选择，不受影响）。
const pythonRuntimeStatus = (item: SleepPluginItem) => {
  if (item.healthState === "ISOLATED" || item.healthState === "ISOLATION_PENDING") {
    const reason = item.healthReason ? `（${item.healthReason}）` : "";
    return `${tm("sleep.runtimePythonIsolatedState")}${reason}`;
  }
  if (item.healthState === "RECOVERY_PENDING") {
    return tm("sleep.runtimePythonRecovery");
  }
  if (item.runtimeCurrent === "python-grpc" && item.runtime === "shared") {
    return tm("sleep.runtimePythonIsolatedState");
  }
  if (item.runtimeCurrent === "python-shared") {
    return tm("sleep.runtimePythonCurrentShared");
  }
  if (item.runtimeCurrent === "python-grpc") {
    return tm("sleep.runtimePythonCurrentGrpc");
  }
  return "";
};

const applyPythonRuntime = async (item: SleepPluginItem, runtime: string) => {
  if (saving.value || !item.id) return;
  saving.value = true;
  try {
    const res = await pluginApi.setRuntimeMode(item.id, runtime);
    if (res.data.status === "ok") {
      item.runtime = runtime;
      toast(tm("sleep.runtimeSaved"));
    } else {
      toast(
        (res.data as any)?.message || tm("messages.operationFailed"),
        "error",
      );
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
  }
};

const requestRuntimeChange = async (item: SleepPluginItem, native: boolean) => {
  if (!native) {
    await applyRuntime(item, false);
    return;
  }
  // 已确认过风险警告则直接切换，否则先弹窗确认并记录确认。
  if (item.nativeConfirmed) {
    await applyRuntime(item, true);
    return;
  }
  runtimeDialog.value = { show: true, item };
};

const confirmRuntimeChange = async () => {
  const item = runtimeDialog.value.item;
  runtimeDialog.value = { show: false, item: null };
  if (!item) return;
  try {
    const res = await pluginApi.confirmNative(item.id);
    if (res.data.status !== "ok") {
      toast(
        (res.data as any)?.message || tm("messages.operationFailed"),
        "error",
      );
      return;
    }
    item.nativeConfirmed = true;
    await applyRuntime(item, true);
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  }
};

onMounted(async () => {
  await fetchData();
});
</script>

<template>
  <div>
    <v-card variant="flat" class="sleep-panel">
      <v-card-text>
        <div class="text-body-2 text-medium-emphasis mb-4">
          {{ tm("sleep.intro") }}
        </div>

        <v-table v-if="plugins.length" class="detail-info-table sleep-table">
          <thead>
            <tr>
              <th>{{ tm("sleep.columnPlugin") }}</th>
              <th>{{ tm("sleep.columnLanguage") }}</th>
              <th>{{ tm("sleep.columnRuntime") }}</th>
              <th>{{ tm("sleep.columnAllow") }}</th>
              <th>{{ tm("sleep.columnMinutes") }}</th>
              <th>{{ tm("sleep.columnWake") }}</th>
              <th>{{ tm("sleep.columnRisk") }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="item in plugins" :key="item.id">
              <td class="sleep-table__name">
                <div class="d-flex align-center ga-2">
                  <span class="text-body-2">{{ item.displayName }}</span>
                  <span class="text-caption text-medium-emphasis">
                    v{{ item.version }}
                  </span>
                </div>
              </td>
              <td>
                <span class="text-body-2">{{ item.language }}</span>
              </td>
              <td class="sleep-table__runtime">
                <template v-if="isGoPlugin(item)">
                  <v-switch
                    :model-value="item.runtime === 'native'"
                    color="primary"
                    density="compact"
                    hide-details
                    :disabled="saving"
                    :label="
                      item.runtime === 'native'
                        ? tm('sleep.runtimeNative')
                        : tm('sleep.runtimeGrpc')
                    "
                    @update:model-value="
                      (v: boolean | null) => requestRuntimeChange(item, !!v)
                    "
                  />
                </template>
                <template v-else-if="isPythonPlugin(item)">
                  <v-select
                    :model-value="item.runtime"
                    :items="pythonRuntimeItems"
                    item-title="title"
                    item-value="value"
                    density="compact"
                    hide-details
                    style="max-width: 200px"
                    :disabled="saving"
                    @update:model-value="(v: string) => applyPythonRuntime(item, v)"
                  />
                  <div v-if="pythonRuntimeStatus(item)" class="text-caption text-medium-emphasis">
                    {{ pythonRuntimeStatus(item) }}
                  </div>
                </template>
                <span v-else class="text-caption text-medium-emphasis">
                  {{ tm("sleep.runtimeOnlyGo") }}
                </span>
              </td>
              <td class="sleep-table__toggle">
                <v-switch
                  :model-value="item.allowSleep"
                  color="primary"
                  density="compact"
                  hide-details
                  :disabled="saving"
                  @update:model-value="(v: boolean | null) => togglePlugin(item, !!v)"
                />
              </td>
              <td>
                <v-text-field
                  v-if="item.allowSleep"
                  type="number"
                  min="0"
                  :model-value="item.idleUnloadMinutes"
                  density="compact"
                  hide-details
                  style="max-width: 110px"
                  :disabled="saving"
                  @change="(v: any) => savePluginMinutes(item, v?.target?.value ?? 0)"
                />
                <span v-else class="text-caption text-medium-emphasis">
                  {{ tm("sleep.residentLabel") }}
                </span>
              </td>
              <td>
                <v-select
                  v-if="item.allowSleep"
                  :model-value="item.idleWakeMode"
                  :items="wakeModeItems"
                  item-title="title"
                  item-value="value"
                  density="compact"
                  hide-details
                  style="max-width: 180px"
                  :disabled="saving"
                  @update:model-value="(v: string) => savePluginWakeMode(item, v)"
                />
                <span v-else class="text-caption text-medium-emphasis">
                  {{ tm("sleep.residentLabel") }}
                </span>
              </td>
              <td>
                <span
                  v-if="item.allowSleep && item.idleWakeMode !== WAKE_HOOK_AND_COMMAND && item.activeEventListener"
                  class="sleep-warning"
                >
                  ⚠️ {{ tm("sleep.commandOnlyRisk") }}
                </span>
              </td>
            </tr>
          </tbody>
        </v-table>
        <div v-else-if="!loading" class="text-body-2 text-medium-emphasis pa-4">
          {{ tm("sleep.noPlugins") }}
        </div>
      </v-card-text>
    </v-card>

    <v-dialog v-model="runtimeDialog.show" max-width="520">
      <v-card>
        <v-card-title class="text-wrap">
          ⚠️ {{ tm("sleep.runtimeSwitchTitle") }}
        </v-card-title>
        <v-card-text>
          <p class="mb-3">{{ tm("sleep.runtimeNativeHint") }}</p>
          <p class="text-medium-emphasis mb-0">
            {{ tm("sleep.runtimeRestartNote") }}
          </p>
        </v-card-text>
        <v-card-actions>
          <v-spacer />
          <v-btn variant="text" @click="runtimeDialog = { show: false, item: null }">
            {{ tm("sleep.runtimeCancel") }}
          </v-btn>
          <v-btn color="primary" variant="flat" @click="confirmRuntimeChange">
            {{ tm("sleep.runtimeSwitchConfirm") }}
          </v-btn>
        </v-card-actions>
      </v-card>
    </v-dialog>

    <v-snackbar
      :timeout="2000"
      elevation="6"
      :color="snackbar.color"
      v-model="snackbar.show"
    >
      {{ snackbar.message }}
    </v-snackbar>
  </div>
</template>

<style scoped>
.sleep-warning {
  color: rgb(var(--v-theme-warning));
  font-size: 12px;
  line-height: 1.5;
}

.sleep-table__name {
  width: 36%;
}

.sleep-table__toggle {
  width: 96px;
}

.sleep-table__runtime {
  min-width: 180px;
  white-space: nowrap;
}
</style>
