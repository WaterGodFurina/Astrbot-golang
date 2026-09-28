<template>
  <v-menu
    v-model="menuOpen"
    :close-on-content-click="false"
    :location="menuLocation"
    offset="10"
    transition="none"
    @update:model-value="handleMenuToggle"
  >
    <template #activator="{ props: menuProps }">
      <button
        v-bind="menuProps"
        class="provider-trigger"
        :class="`provider-trigger--${variant}`"
        type="button"
      >
        <span class="provider-trigger-copy">
          <span class="provider-trigger-title">{{ triggerTitle }}</span>
          <span v-if="triggerMeta" class="provider-trigger-meta">
            {{ triggerMeta }}
          </span>
        </span>
        <v-icon class="provider-trigger-chevron" size="18">
          mdi-chevron-down
        </v-icon>
      </button>
    </template>

    <v-card class="provider-menu-card" elevation="0">
      <div class="provider-menu-body">
        <div class="provider-search-row">
          <v-menu
            v-if="providerSources.length > 1"
            v-model="sourceMenuOpen"
            :close-on-content-click="false"
            offset="6"
            transition="none"
          >
            <template #activator="{ props: sourceMenuProps }">
              <button
                v-bind="sourceMenuProps"
                type="button"
                class="provider-source-trigger"
                :title="sharedTm('providerSelector.filterSource')"
              >
                <span>{{
                  selectedSourceId || sharedTm("providerSelector.allSources")
                }}</span>
                <v-icon size="16">mdi-chevron-down</v-icon>
              </button>
            </template>
            <v-card class="provider-source-menu" elevation="0">
              <v-list density="compact" nav>
                <v-list-item
                  :active="!selectedSourceId"
                  @click="
                    selectedSourceId = '';
                    sourceMenuOpen = false;
                  "
                >
                  <v-list-item-title>{{
                    sharedTm("providerSelector.allSources")
                  }}</v-list-item-title>
                </v-list-item>
                <v-list-item
                  v-for="source in providerSources"
                  :key="source.id"
                  :active="selectedSourceId === source.id"
                  @click="
                    selectedSourceId = source.id;
                    sourceMenuOpen = false;
                  "
                >
                  <v-list-item-title>{{ source.id }}</v-list-item-title>
                  <v-list-item-subtitle
                    v-if="source.apiBase"
                    :title="source.apiBase"
                    >{{ source.apiBase }}</v-list-item-subtitle
                  >
                </v-list-item>
              </v-list>
            </v-card>
          </v-menu>
          <v-text-field
            v-model="searchQuery"
            placeholder="Search models"
            hide-details
            variant="outlined"
            density="compact"
            prepend-inner-icon="mdi-magnify"
            class="provider-search"
            clearable
          />
        </div>

        <v-list
          ref="providerListRef"
          density="compact"
          nav
          class="provider-menu-list"
          @scroll.passive="providerScrollTop = $event.target.scrollTop"
        >
          <div
            :style="{ height: `${visibleProviderGroups[0]?.top || 0}px` }"
            aria-hidden="true"
          />
          <div
            v-for="group in visibleProviderGroups"
            :key="group.id"
            class="provider-source-group"
          >
            <v-list-subheader class="provider-source-header">
              {{ group.id }}
            </v-list-subheader>
            <div
              :style="{ height: `${group.paddingTop}px` }"
              aria-hidden="true"
            />
            <v-list-item
              v-for="provider in group.providers"
              :key="provider.id"
              :data-selected="selectedProviderId === provider.id || undefined"
              :active="selectedProviderId === provider.id"
              rounded="lg"
              class="provider-menu-item"
              @click="selectProvider(provider)"
            >
              <v-list-item-title class="provider-item-title">
                {{ provider.id }}
              </v-list-item-title>
              <v-list-item-subtitle class="provider-subtitle">
                <span class="model-name">{{ provider.model }}</span>
                <span class="meta-icons">
                  <v-tooltip
                    v-for="item in capabilityBadges(provider)"
                    :key="item.key"
                    location="top"
                    max-width="320"
                  >
                    <template #activator="{ props: badgeTooltipProps }">
                      <span
                        v-bind="badgeTooltipProps"
                        class="meta-icon-badge"
                        :class="{ 'meta-icon-badge--disabled': !item.enabled }"
                        @click.stop
                      >
                        <v-icon size="13">{{ item.icon }}</v-icon>
                      </span>
                    </template>
                    <span>{{ item.tooltip }}</span>
                  </v-tooltip>
                  <v-tooltip
                    v-if="formatContextLimit(provider, metadataForProvider(provider))"
                    location="top"
                    max-width="320"
                  >
                    <template #activator="{ props: contextTooltipProps }">
                      <span
                        v-bind="contextTooltipProps"
                        class="meta-context-badge"
                        @click.stop
                      >
                        {{ formatContextLimit(provider, metadataForProvider(provider)) }}
                      </span>
                    </template>
                    <span>{{
                      tm("models.metadata.context", {
                        tokens: formatContextLimit(
                          provider,
                          metadataForProvider(provider),
                        ),
                      })
                    }}</span>
                  </v-tooltip>
                </span>
              </v-list-item-subtitle>
              <template #append>
                <div class="provider-menu-actions" @click.stop>
                  <v-tooltip location="top">
                    <template #activator="{ props: testTooltipProps }">
                      <v-btn
                        v-bind="testTooltipProps"
                        icon="mdi-connection"
                        size="x-small"
                        variant="text"
                        :loading="testingProviderIds.includes(provider.id)"
                        :disabled="testingProviderIds.includes(provider.id)"
                        @click.stop="testProvider(provider)"
                      />
                    </template>
                    <span>{{ tm("models.testButton") }}</span>
                  </v-tooltip>
                  <v-icon
                    v-if="selectedProviderId === provider.id"
                    class="provider-selected-icon"
                    size="18"
                  >
                    mdi-check
                  </v-icon>
                </div>
              </template>
            </v-list-item>
            <div
              :style="{ height: `${group.paddingBottom}px` }"
              aria-hidden="true"
            />
          </div>
          <div
            :style="{
              height: `${
                (providerGroups[providerGroups.length - 1]?.bottom || 0) -
                (visibleProviderGroups[visibleProviderGroups.length - 1]
                  ?.bottom || 0)
              }px`,
            }"
            aria-hidden="true"
          />
        </v-list>

        <div v-if="loadingProviders" class="empty-hint">
          Loading models...
        </div>
        <div v-else-if="filteredProviders.length === 0" class="empty-hint">
          No available models
        </div>
      </div>
    </v-card>
  </v-menu>
</template>

<script setup lang="ts">
import { ref, computed, nextTick, onMounted, shallowRef, watch } from "vue";
import { useDisplay } from "vuetify";
import { providerApi } from "@/api/v1";
import { useModuleI18n } from "@/i18n/composables";
import { useToast } from "@/utils/toast";
import {
  formatContextLimit,
  providerCapabilityBadges,
  type ProviderModelMetadata,
  type ProviderMetadataSource,
} from "@/utils/providerMetadata";

interface ProviderConfig extends ProviderMetadataSource {
  id: string;
  model: string;
  type?: string;
  provider_type?: string;
  provider_source_id?: string;
  api_base?: string;
  embedding_api_base?: string;
  rerank_api_base?: string;
  gemini_tts_api_base?: string;
  enable?: boolean;
}

const props = withDefaults(
  defineProps<{
    variant?: "input" | "header";
  }>(),
  {
    variant: "input",
  },
);

const SELECTED_PROVIDER_KEY = "selectedProvider";
const SELECTED_PROVIDER_MODEL_KEY = "selectedProviderModel";
const providerConfigs = shallowRef<ProviderConfig[]>([]);
const selectedProviderId = ref("");
const selectedModelName = ref("");
const searchQuery = ref("");
const menuOpen = ref(false);
const sourceMenuOpen = ref(false);
const selectedSourceId = ref("");
const providerScrollTop = ref(0);
const { height: displayHeight } = useDisplay();
const providerListHeight = computed(() =>
  Math.min(360, displayHeight.value * 0.58),
);
// Keep these sizes in sync with the model rows and sticky source headers below.
const PROVIDER_ROW_HEIGHT = 56;
const SOURCE_HEADER_HEIGHT = 32;
let scrollToSelectionPending = false;
const providerListRef = ref<{ $el: HTMLElement } | null>(null);
const loadingProviders = ref(false);
const providersLoaded = ref(false);
const testingProviderIds = ref<string[]>([]);
const modelMetadata = shallowRef<Record<string, ProviderModelMetadata>>({});
const { tm } = useModuleI18n("features/provider");
const { tm: sharedTm } = useModuleI18n("core.shared");
const { success: toastSuccess, error: toastError } = useToast();

const variant = computed(() => props.variant);
const menuLocation = computed(() =>
  props.variant === "header" ? "bottom start" : "top",
);

const selectedProvider = computed(() =>
  providerConfigs.value.find(
    (provider) => provider.id === selectedProviderId.value,
  ),
);

const triggerTitle = computed(() => {
  if (selectedProvider.value?.id) return selectedProvider.value.id;
  if (selectedProviderId.value) return selectedProviderId.value;
  return props.variant === "header" ? "Default model" : "Model";
});

const triggerMeta = computed(() => {
  const model = selectedProvider.value?.model || selectedModelName.value;
  if (!model || model === triggerTitle.value) return "";
  return model;
});

const filteredProviders = computed(() => {
  const query = (searchQuery.value || "").trim().toLowerCase();
  return providerConfigs.value.filter(
    (provider) =>
      (!selectedSourceId.value ||
        (provider.provider_source_id || provider.type || provider.id) ===
          selectedSourceId.value) &&
      (!query ||
        provider.id.toLowerCase().includes(query) ||
        (provider.provider_source_id || "").toLowerCase().includes(query) ||
        String(provider.model || "")
          .toLowerCase()
          .includes(query)),
  );
});

const providerSources = computed(() => {
  const sources = new Map<string, { id: string; apiBase: string }>();
  for (const provider of providerConfigs.value) {
    const id = provider.provider_source_id || provider.type || provider.id;
    if (!sources.has(id))
      sources.set(id, {
        id,
        apiBase:
          provider.api_base ||
          provider.embedding_api_base ||
          provider.rerank_api_base ||
          provider.gemini_tts_api_base ||
          "",
      });
  }
  return [...sources.values()];
});

const providerGroups = computed(() => {
  const groups = new Map<string, ProviderConfig[]>();
  for (const provider of filteredProviders.value) {
    const sourceId =
      provider.provider_source_id || provider.type || provider.id;
    const group = groups.get(sourceId);
    if (group) group.push(provider);
    else groups.set(sourceId, [provider]);
  }
  let offset = 0;
  return Array.from(groups, ([id, providers]) => {
    const top = offset;
    offset += SOURCE_HEADER_HEIGHT + providers.length * PROVIDER_ROW_HEIGHT;
    return { id, providers, top, bottom: offset };
  });
});

const visibleProviderGroups = computed(() => {
  // Spacers preserve full group heights for sticky headers while only nearby rows mount.
  const scrollTop = providerScrollTop.value;
  const start = Math.max(0, scrollTop - PROVIDER_ROW_HEIGHT * 3);
  const end = scrollTop + providerListHeight.value + PROVIDER_ROW_HEIGHT * 3;
  return providerGroups.value
    .filter((group) => group.bottom >= start && group.top <= end)
    .map((group) => {
      const first = Math.max(
        0,
        Math.min(
          group.providers.length,
          Math.floor(
            (start - group.top - SOURCE_HEADER_HEIGHT) / PROVIDER_ROW_HEIGHT,
          ),
        ),
      );
      const last = Math.max(
        first,
        Math.min(
          group.providers.length,
          Math.ceil(
            (end - group.top - SOURCE_HEADER_HEIGHT) / PROVIDER_ROW_HEIGHT,
          ),
        ),
      );
      return {
        ...group,
        providers: group.providers.slice(first, last),
        paddingTop: first * PROVIDER_ROW_HEIGHT,
        paddingBottom: (group.providers.length - last) * PROVIDER_ROW_HEIGHT,
      };
    });
});

function loadFromStorage() {
  const savedProvider = localStorage.getItem(SELECTED_PROVIDER_KEY);
  const savedModel = localStorage.getItem(SELECTED_PROVIDER_MODEL_KEY);
  if (savedProvider) {
    selectedProviderId.value = savedProvider;
  }
  if (savedModel) {
    selectedModelName.value = savedModel;
  }
}

function saveToStorage(provider: ProviderConfig) {
  localStorage.setItem(SELECTED_PROVIDER_KEY, provider.id);
  localStorage.setItem(SELECTED_PROVIDER_MODEL_KEY, provider.model || "");
}

async function loadProviderConfigs(force = false) {
  if (loadingProviders.value || (providersLoaded.value && !force)) return;
  loadingProviders.value = true;
  try {
    const response = await providerApi.listByProviderType("chat_completion");
    if (response.data.status === "ok") {
      modelMetadata.value = (
        response.data.model_metadata || {}
      ) as Record<string, ProviderModelMetadata>;
      providerConfigs.value = (
        (response.data.data || []) as unknown as ProviderConfig[]
      ).filter((provider: ProviderConfig) => provider.enable !== false);
      providersLoaded.value = true;
      if (
        selectedSourceId.value &&
        !providerSources.value.some(
          (source) => source.id === selectedSourceId.value,
        )
      ) {
        selectedSourceId.value = "";
      }
      const selected = selectedProvider.value;
      if (selected) {
        selectedModelName.value = selected.model || "";
        saveToStorage(selected);
      }
    }
  } catch (error) {
    console.error("Failed to load provider list:", error);
    if (!providersLoaded.value) providerConfigs.value = [];
  } finally {
    loadingProviders.value = false;
  }
}

function selectProvider(provider: ProviderConfig) {
  selectedProviderId.value = provider.id;
  selectedModelName.value = provider.model || "";
  saveToStorage(provider);
  menuOpen.value = false;
}

function capabilityBadges(provider: ProviderConfig) {
  return providerCapabilityBadges(provider, metadataForProvider(provider), tm);
}

function metadataForProvider(provider: ProviderConfig) {
  return provider.model ? modelMetadata.value[provider.model] || null : null;
}

async function testProvider(provider: ProviderConfig) {
  if (testingProviderIds.value.includes(provider.id)) return;
  testingProviderIds.value.push(provider.id);
  try {
    const startTime = performance.now();
    const response = await providerApi.test(provider.id);
    if (response.data.status === "ok" && response.data.data.error === null) {
      const latency = Math.max(0, Math.round(performance.now() - startTime));
      toastSuccess(
        tm("models.testSuccessWithLatency", {
          id: provider.id,
          latency,
        }),
      );
    } else {
      throw new Error(response.data.data.error || tm("models.testError"));
    }
  } catch (error: any) {
    toastError(
      error.response?.data?.message || error.message || tm("models.testError"),
    );
  } finally {
    testingProviderIds.value = testingProviderIds.value.filter(
      (id) => id !== provider.id,
    );
  }
}

function getCurrentSelection() {
  return {
    providerId: selectedProviderId.value,
    modelName: selectedProvider.value?.model || selectedModelName.value || "",
  };
}

function handleMenuToggle(isOpen: boolean) {
  if (isOpen) {
    scrollToSelectionPending = true;
    searchQuery.value = "";
    selectedSourceId.value = "";
    loadProviderConfigs(true);
  }
}

watch(
  [menuOpen, loadingProviders, searchQuery, selectedSourceId],
  async ([isOpen, loading, query, source], [, , oldQuery, oldSource]) => {
    if (!isOpen || (loading && !providersLoaded.value)) return;
    await nextTick();
    const list = providerListRef.value?.$el;
    if (!list) return;
    if (scrollToSelectionPending) {
      scrollToSelectionPending = false;
      let target = 0;
      for (const group of providerGroups.value) {
        const index = group.providers.findIndex(
          (provider) => selectedProviderId.value === provider.id,
        );
        if (index < 0) continue;
        target = Math.max(
          0,
          group.top +
            SOURCE_HEADER_HEIGHT +
            index * PROVIDER_ROW_HEIGHT -
            (list.clientHeight - PROVIDER_ROW_HEIGHT) / 2,
        );
        break;
      }
      list.scrollTop = target;
      providerScrollTop.value = list.scrollTop;
    } else if (query !== oldQuery || source !== oldSource) {
      list.scrollTop = 0;
      providerScrollTop.value = 0;
    }
  },
);

onMounted(() => {
  loadFromStorage();
  loadProviderConfigs();
});

defineExpose({
  getCurrentSelection,
});
</script>

<style scoped>
.provider-trigger {
  display: inline-flex;
  max-width: 100%;
  min-width: 0;
  align-items: center;
  justify-content: flex-start;
  gap: 6px;
  border: 0;
  background: transparent;
  color: rgb(var(--v-theme-on-surface));
  cursor: pointer;
  font: inherit;
  letter-spacing: 0;
  text-align: left;
}

.provider-trigger-copy {
  display: inline-flex;
  min-width: 0;
  align-items: baseline;
  gap: 10px;
}

.provider-trigger-title,
.provider-trigger-meta {
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.provider-trigger--header {
  height: 24px;
  margin-top: 2px;
  padding: 0;
}

.provider-trigger--header .provider-trigger-title {
  font-size: 17px;
  font-weight: 620;
  line-height: 24px;
}

.provider-trigger--header .provider-trigger-meta {
  color: rgba(var(--v-theme-on-surface), 0.55);
  font-size: 13px;
  font-weight: 500;
  line-height: 18px;
}

.provider-trigger--input {
  height: 40px;
  max-width: min(280px, 42vw);
  padding: 0 12px;
  border: 1px solid rgba(var(--v-theme-on-surface), 0.18);
  border-radius: 999px;
}

.provider-trigger--input:hover {
  border-color: rgba(var(--v-theme-on-surface), 0.34);
  background: rgba(var(--v-theme-on-surface), 0.04);
}

.provider-trigger--input .provider-trigger-title {
  font-size: 14px;
  font-weight: 500;
}

.provider-trigger--input .provider-trigger-meta {
  display: none;
}

.provider-trigger-chevron {
  flex: 0 0 auto;
  opacity: 0.64;
}

.provider-menu-card {
  width: min(420px, calc(100vw - 24px));
  overflow: hidden;
  border: 0;
  border-radius: 14px !important;
  background: rgb(var(--v-theme-surface));
  box-shadow: 0 12px 28px rgba(0, 0, 0, 0.08) !important;
}

.provider-menu-body {
  padding: 10px;
}

.provider-search-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

.provider-search {
  flex: 1;
  min-width: 0;
}

.provider-source-trigger {
  display: flex;
  align-items: center;
  gap: 4px;
  max-width: 135px;
  height: 40px;
  padding: 0 8px;
  border: 1px solid rgba(var(--v-theme-on-surface), 0.16);
  border-radius: 10px;
  color: rgb(var(--v-theme-on-surface));
  font-size: 12px;
}

.provider-source-trigger span {
  overflow: hidden;
  white-space: nowrap;
  text-overflow: ellipsis;
}

.provider-source-menu {
  width: min(360px, calc(100vw - 24px));
  max-height: 320px;
  overflow-y: auto;
  border: 1px solid rgba(var(--v-theme-on-surface), 0.09);
  border-radius: 12px !important;
}

.provider-search :deep(.v-field) {
  border-radius: 10px;
  box-shadow: none;
}

.provider-search :deep(.v-field .v-field__outline) {
  --v-field-border-opacity: 1;
  color: rgba(var(--v-theme-on-surface), 0.16);
}

.provider-menu-list {
  max-height: min(360px, 58vh);
  overflow-y: auto;
  padding: 0;
}

.provider-menu-item {
  height: 54px;
  min-height: 54px !important;
  margin: 0 0 2px !important;
  border-radius: 10px !important;
}

.provider-source-group {
  display: flow-root;
}

.provider-source-header {
  position: sticky;
  top: 0;
  z-index: 1;
  height: 32px;
  min-height: 32px;
  background: rgb(var(--v-theme-surface));
  font-size: 12px;
  font-weight: 600;
}

.provider-menu-item:hover {
  background: rgba(var(--v-theme-on-surface), 0.05);
}

.provider-menu-item.v-list-item--active {
  background: rgba(var(--v-theme-primary), 0.08);
  color: rgb(var(--v-theme-on-surface));
}

.provider-item-title {
  font-size: 14px;
  font-weight: 600;
  line-height: 18px;
}

.provider-subtitle {
  display: flex;
  min-width: 0;
  align-items: center;
  gap: 8px;
}

.model-name {
  min-width: 0;
  overflow: hidden;
  color: rgba(var(--v-theme-on-surface), 0.58);
  font-size: 12px;
  line-height: 16px;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.meta-icons {
  display: flex;
  flex: 0 0 auto;
  align-items: center;
  gap: 4px;
  color: rgba(var(--v-theme-on-surface), 0.5);
}

.meta-icon-badge {
  display: inline-flex;
  align-items: center;
  color: rgba(var(--v-theme-on-surface), 0.72);
}

.meta-icon-badge--disabled {
  color: rgba(var(--v-theme-on-surface), 0.34);
}

.meta-context-badge {
  display: inline-flex;
  align-items: center;
  height: 16px;
  padding: 0 5px;
  border-radius: 999px;
  background: rgba(var(--v-theme-on-surface), 0.06);
  color: rgba(var(--v-theme-on-surface), 0.72);
  font-size: 10px;
  font-weight: 650;
  line-height: 16px;
}

.provider-menu-actions {
  display: inline-flex;
  align-items: center;
  gap: 2px;
}

.provider-selected-icon {
  color: rgba(var(--v-theme-on-surface), 0.72);
}

.empty-hint {
  padding: 16px;
  color: rgba(var(--v-theme-on-surface), 0.5);
  font-size: 12px;
  text-align: center;
}

@media (max-width: 768px) {
  .provider-trigger--header .provider-trigger-title {
    font-size: 16px;
  }

  .provider-trigger--header .provider-trigger-meta {
    display: none;
  }

  .provider-trigger--input {
    height: 38px;
    max-width: 48vw;
  }
}
</style>
