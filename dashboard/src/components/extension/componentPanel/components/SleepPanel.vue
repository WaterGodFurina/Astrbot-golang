<script setup lang="ts">
/**
 * 休眠策略面板 - 组件管理页第三个视图（与"指令/函数工具"按钮同列）。
 * 全局闲置自动休眠开关 + 每个插件的"允许休眠"开关（关闭 = 常驻）。
 */
import { onMounted, ref } from "vue";
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
  blocked: boolean;
  version: string;
}

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

const loading = ref(false);
const saving = ref(false);
const globalMinutes = ref(0);
const plugins = ref<SleepPluginItem[]>([]);
const snackbar = ref<{ show: boolean; message: string; color: string }>({
  show: false,
  message: "",
  color: "success",
});

const globalEnabled = () => globalMinutes.value > 0;

const toast = (message: string, color = "success") => {
  snackbar.value.message = message;
  snackbar.value.color = color;
  snackbar.value.show = true;
};

const fetchData = async () => {
  loading.value = true;
  try {
    // 全局阈值（分钟）：openapi 客户端只有 POST 变体，GET 走原始请求。
    const globalRes = await fetchWithAuth("/api/v1/plugins/idle-unload-global", {
      method: "GET",
    });
    const globalJson = await globalRes.json().catch(() => null);
    if (globalJson?.status === "ok") {
      globalMinutes.value = globalJson.data?.minutes || 0;
    }
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
          blocked: Boolean(p.idle_unload_blocked),
          version: String(p.version || ""),
        }));
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    loading.value = false;
  }
};

const toggleGlobal = async (enabled: boolean) => {
  if (saving.value) return;
  saving.value = true;
  try {
    const minutes = enabled ? 10 : 0;
    const res = await pluginApi.setGlobalIdleSleep(minutes);
    if (res.data.status === "ok") {
      globalMinutes.value = minutes;
      toast(tm("sleep.globalSaved"));
    } else {
      toast((res.data as any)?.message || tm("messages.operationFailed"), "error");
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
  }
};

const togglePlugin = async (item: SleepPluginItem, allowSleep: boolean) => {
  if (saving.value || !item.id) return;
  saving.value = true;
  try {
    const res = await pluginApi.setIdleSleep(item.id, allowSleep);
    if (res.data.status === "ok") {
      item.blocked = !allowSleep;
      toast(tm("sleep.pluginSaved"));
    } else {
      toast((res.data as any)?.message || tm("messages.operationFailed"), "error");
    }
  } catch (err) {
    toast((err as any)?.message || String(err), "error");
  } finally {
    saving.value = false;
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
        <div class="d-flex align-center ga-3 mb-2 flex-wrap">
          <v-switch
            :model-value="globalEnabled()"
            color="primary"
            density="compact"
            hide-details
            :disabled="saving"
            :loading="loading"
            @update:model-value="(v: boolean | null) => toggleGlobal(!!v)"
          />
          <div>
            <div class="text-body-1 font-weight-medium">
              {{ tm("sleep.globalLabel") }}
            </div>
            <div class="text-caption text-medium-emphasis">
              {{ tm("sleep.globalDesc") }}
            </div>
          </div>
        </div>
        <div class="sleep-warning mb-4">
          {{ tm("sleep.warning") }}
        </div>

        <v-table v-if="plugins.length" class="detail-info-table sleep-table">
          <thead>
            <tr>
              <th>{{ tm("sleep.columnPlugin") }}</th>
              <th>{{ tm("sleep.columnLanguage") }}</th>
              <th>{{ tm("sleep.columnAllow") }}</th>
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
              <td class="sleep-table__toggle">
                <v-switch
                  :model-value="!item.blocked"
                  color="primary"
                  density="compact"
                  hide-details
                  :disabled="saving || !globalEnabled()"
                  @update:model-value="(v: boolean | null) => togglePlugin(item, !!v)"
                />
              </td>
            </tr>
          </tbody>
        </v-table>
        <div v-else-if="!loading" class="text-body-2 text-medium-emphasis pa-4">
          {{ tm("sleep.noPlugins") }}
        </div>
      </v-card-text>
    </v-card>

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
  width: 40%;
}

.sleep-table__toggle {
  width: 120px;
}
</style>
