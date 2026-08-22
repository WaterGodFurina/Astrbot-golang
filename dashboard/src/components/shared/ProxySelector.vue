<template>
    <div class="proxy-selector">
        <h5 class="proxy-selector__title">{{ tm('network.proxySelector.title') }}</h5>
        <v-radio-group class="proxy-selector__mode mt-2" v-model="radioValue" hide-details="true">
            <v-radio :label="tm('network.proxySelector.noProxy')" value="0"></v-radio>
            <v-radio value="1">
                <template v-slot:label>
                    <span>{{ tm('network.proxySelector.useProxy') }}</span>
                    <v-btn v-if="radioValue === '1'" class="ml-2" @click="testAllProxies" size="x-small"
                        variant="tonal" :loading="loadingTestingConnection">
                        {{ tm('network.proxySelector.testConnection') }}
                    </v-btn>
                </template>
            </v-radio>
        </v-radio-group>
        <v-expand-transition>
            <div v-if="radioValue === '1'" class="proxy-selector__list">
                <v-radio-group v-model="githubProxyRadioControl" class="mt-2" hide-details="true">
                    <v-radio color="success" v-for="(proxy, idx) in githubProxies" :key="proxy" :value="String(idx)">
                        <template v-slot:label>
                            <div class="proxy-selector__option-label">
                                <span class="proxy-selector__url">{{ proxy }}</span>
                                <div v-if="proxyStatus[idx]" class="proxy-selector__status">
                                    <v-chip
                                        :color="proxyStatus[idx].available ? 'success' : 'error'"
                                        size="x-small"
                                        class="mr-1">
                                        {{ proxyStatus[idx].available ? tm('network.proxySelector.available') : tm('network.proxySelector.unavailable') }}
                                    </v-chip>
                                    <v-chip
                                        v-if="proxyStatus[idx].available"
                                        color="info"
                                        size="x-small">
                                        {{ proxyStatus[idx].latency }}ms
                                    </v-chip>
                                </div>
                            </div>
                        </template>
                    </v-radio>
                    <v-radio color="primary" value="-1" :label="tm('network.proxySelector.custom')">
                        <template v-slot:label v-if="String(githubProxyRadioControl) === '-1'">
                            <v-text-field class="proxy-selector__custom-input" density="compact" v-model="selectedGitHubProxy" variant="outlined"
                                :placeholder="tm('network.proxySelector.custom')" hide-details="true">
                            </v-text-field>
                        </template>
                    </v-radio>
                </v-radio-group>
            </div>
        </v-expand-transition>
    </div>
</template>


<script>
import { statsApi } from '@/api/v1';
import { useModuleI18n } from '@/i18n/composables';

export default {
    props: {
        // github_proxy 配置值（config 持久化，优先于 localStorage）
        modelValue: { type: String, default: "" },
    },
    emits: ["update:modelValue"],
    setup() {
        const { tm } = useModuleI18n('features/settings');
        return { tm };
    },
    data() {
        return {
            githubProxies: [
                "https://edgeone.gh-proxy.com",
                "https://hk.gh-proxy.com",
                "https://gh-proxy.com",
                "https://gh.dpik.top",
            ],
            githubProxyRadioControl: "0", // the index of the selected proxy
            selectedGitHubProxy: "",
            radioValue: "0", // 0: 不使用, 1: 使用
            loadingTestingConnection: false,
            testingProxies: {},
            proxyStatus: {},
            initializing: true,
        }
    },
    methods: {
        getProxyByControl(control) {
            const normalizedControl = String(control);
            if (normalizedControl === "-1") {
                return "";
            }
            const index = Number.parseInt(normalizedControl, 10);
            if (Number.isNaN(index)) {
                return "";
            }
            return this.githubProxies[index] || "";
        },
        async testSingleProxy(idx) {
            this.testingProxies[idx] = true;
            
            const proxy = this.githubProxies[idx];
            
            try {
                const response = await statsApi.testGhproxy({
                    proxy_url: proxy
                });
                console.log(response.data);
                const latency = Number(response.data.data?.latency);
                if (response.status === 200 && Number.isFinite(latency) && latency >= 0) {
                    this.proxyStatus[idx] = {
                        available: true,
                        latency: Math.round(latency)
                    };
                } else {
                    this.proxyStatus[idx] = {
                        available: false,
                        latency: 0
                    };
                }
            } catch (error) {
                this.proxyStatus[idx] = {
                    available: false,
                    latency: 0
                };
            } finally {
                this.testingProxies[idx] = false;
            }
        },
        
        async testAllProxies() {
            this.loadingTestingConnection = true;
            
            const promises = this.githubProxies.map((proxy, idx) => 
                this.testSingleProxy(idx)
            );
            
            await Promise.all(promises);
            this.loadingTestingConnection = false;
        },
    },
    mounted() {
        this.initializing = true;

        const savedProxy = this.modelValue || localStorage.getItem('selectedGitHubProxy') || "";
        const savedRadio = localStorage.getItem('githubProxyRadioValue');
        const savedControl = String(localStorage.getItem('githubProxyRadioControl') || "0");

        // 默认选中项来自设置里的 github_proxy（commonStore.githubProxyConfig，
        // 由打开弹窗的调用方预先拉取传入 modelValue）；用户在弹窗里做过选择
        // （localStorage 有记录）则以用户选择为准。
        if (savedRadio === null && !savedProxy) {
            const configured = this.modelValue || "";
            if (configured) {
                // 设置值匹配预设列表 → 选中对应 radio；否则选中自定义并填入。
                const idx = this.githubProxies.indexOf(configured);
                this.radioValue = "1";
                if (idx >= 0) {
                    this.githubProxyRadioControl = String(idx);
                    this.selectedGitHubProxy = configured;
                } else {
                    this.githubProxyRadioControl = "-1";
                    this.selectedGitHubProxy = configured;
                }
                this.initializing = false;
                this.$emit("update:modelValue", configured);
                return;
            }
        }

        this.radioValue = savedRadio || "0";
        this.githubProxyRadioControl = savedControl;

        if (this.radioValue === "1") {
            if (savedControl !== "-1") {
                this.selectedGitHubProxy = this.getProxyByControl(savedControl);
            } else {
                this.selectedGitHubProxy = savedProxy;
            }
        } else {
            this.selectedGitHubProxy = "";
        }

        this.initializing = false;
    },
    watch: {
        selectedGitHubProxy: function (newVal, oldVal) {
            if (this.initializing) {
                return;
            }
            if (!newVal) {
                newVal = "";
            }
            localStorage.setItem('selectedGitHubProxy', newVal);
            this.$emit('update:modelValue', newVal);
        },
        radioValue: function (newVal) {
            if (this.initializing) {
                return;
            }
            localStorage.setItem('githubProxyRadioValue', newVal);
            if (String(newVal) === "0") {
                this.selectedGitHubProxy = "";
            } else if (String(this.githubProxyRadioControl) !== "-1") {
                this.selectedGitHubProxy = this.getProxyByControl(this.githubProxyRadioControl);
            }
        },
        githubProxyRadioControl: function (newVal) {
            if (this.initializing) {
                return;
            }
            const normalizedVal = String(newVal);
            localStorage.setItem('githubProxyRadioControl', normalizedVal);
            if (String(this.radioValue) !== "1") {
                this.selectedGitHubProxy = "";
                return;
            }
            if (normalizedVal !== "-1") {
                this.selectedGitHubProxy = this.getProxyByControl(normalizedVal);
            }
        }
    }
}
</script>

<style scoped>
.proxy-selector {
    width: 100%;
    min-width: 0;
}

.proxy-selector__title {
    margin: 0;
    color: rgb(var(--v-theme-on-surface));
    font-size: 0.88rem;
    font-weight: 700;
    line-height: 1.4;
}

.proxy-selector__list {
    margin-left: 16px;
}

.proxy-selector :deep(.v-label) {
    min-width: 0;
    font-size: 0.875rem;
    line-height: 1.35;
    white-space: normal;
}

.proxy-selector__option-label {
    display: flex;
    align-items: center;
    gap: 8px;
    min-width: 0;
    max-width: 100%;
}

.proxy-selector__url {
    overflow-wrap: anywhere;
    word-break: normal;
}

.proxy-selector__status {
    display: flex;
    flex: 0 0 auto;
    align-items: center;
}

.proxy-selector__custom-input {
    width: min(100%, 420px);
}
</style>
