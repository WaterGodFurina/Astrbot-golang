<script setup>
import ConsoleDisplayer from '@/components/shared/ConsoleDisplayer.vue';
import { useModuleI18n } from '@/i18n/composables';
import { updatesApi } from '@/api/v1';
import { useToast } from '@/utils/toast';

const { tm } = useModuleI18n('features/console');
</script>

<template>
  <div class="console-page">
    <div class="console-header">
      <div>
        <h1 class="text-h2 mb-1">{{ tm('title') }}</h1>
        <p class="text-body-2 text-medium-emphasis mb-0">
          {{ tm('debugHint.text') }}
        </p>
      </div>
      <div class="d-flex align-center">
        <v-switch
          v-model="hideUserChatEnabled"
          :label="hideUserChatEnabled ? tm('hideUserChat.enabled') : tm('hideUserChat.disabled')"
          hide-details
          density="compact"
          inset
          color="primary"
          style="margin-right: 16px;"
        ></v-switch>
        <v-switch
          v-model="autoScrollEnabled"
          :label="autoScrollEnabled ? tm('autoScroll.enabled') : tm('autoScroll.disabled')"
          hide-details
          density="compact"
          inset
          color="primary"
          style="margin-right: 16px;"
        ></v-switch>
        <v-dialog v-model="goInstallDialog" width="400">
          <template v-slot:activator="{ props }">
            <v-btn variant="text" v-bind="props">{{ tm('goInstall.button') }}</v-btn>
          </template>
          <v-card>
            <v-card-title class="text-h3 pa-4 pb-0 pl-6">
              <span>{{ tm('goInstall.dialogTitle') }}</span>
            </v-card-title>
            <v-card-text>
              <v-text-field v-model="goInstallPayload.package" :label="tm('goInstall.packageLabel')" variant="outlined"></v-text-field>
              <v-text-field v-model="goInstallPayload.mirror" :label="tm('goInstall.mirrorLabel')" variant="outlined"></v-text-field>
              <small>{{ tm('goInstall.mirrorHint') }}</small>
            </v-card-text>
            <v-card-actions>
              <v-spacer></v-spacer>
              <v-btn color="blue-darken-1" variant="text" @click="goInstall" :loading="loading">
                {{ tm('goInstall.installButton') }}
              </v-btn>
            </v-card-actions>
          </v-card>
        </v-dialog>
      </div>
    </div>
    <ConsoleDisplayer ref="consoleDisplayer" class="console-display" :hide-user-chat="hideUserChatEnabled" />
  </div>
</template>
<script>
export default {
  name: 'ConsolePage',
  components: {
    ConsoleDisplayer
  },
  data() {
    return {
      autoScrollEnabled: localStorage.getItem('console_auto_scroll') !== 'false',
      hideUserChatEnabled: localStorage.getItem('console_hide_user_chat') === 'true',
      goInstallDialog: false,
      goInstallPayload: {
        package: '',
        mirror: ''
      },
      loading: false
    }
  },
  mounted() {
    if (this.$refs.consoleDisplayer) {
      this.$refs.consoleDisplayer.autoScroll = this.autoScrollEnabled;
    }
  },
  watch: {
    autoScrollEnabled(val) {
      localStorage.setItem('console_auto_scroll', val);
      if (this.$refs.consoleDisplayer) {
        this.$refs.consoleDisplayer.autoScroll = val;
      }
    },
    hideUserChatEnabled(val) {
      localStorage.setItem('console_hide_user_chat', val);
    }
  },
  methods: {
    goInstall() {
      const toast = useToast();
      this.loading = true;
      updatesApi.installPip(this.goInstallPayload)
        .then(res => {
          if (res.data.status === 'ok') {
            toast.success(res.data.message || tm('goInstall.installSuccess'));
            this.goInstallDialog = false;
          } else {
            toast.error(res.data.message || tm('goInstall.installFailed'));
          }
        })
        .catch(err => {
          toast.error(err.response?.data?.message || tm('goInstall.requestFailed'));
        }).finally(() => {
          this.loading = false;
        });
    }
  }
}

</script>

<style scoped>
.console-page {
  display: flex;
  flex-direction: column;
  height: calc(100vh - 67px);
  margin: 0 auto;
  max-width: 1400px;
  padding: 24px;
  width: 100%;
}

.console-header {
  align-items: flex-start;
  display: flex;
  flex-shrink: 0;
  justify-content: space-between;
  margin-bottom: 24px;
}

.console-display {
  flex: 1;
  min-height: 0;
  width: 100%;
}

@keyframes fadeIn {
  from {
    opacity: 0;
  }

  to {
    opacity: 1;
  }
}

.fade-in {
  animation: fadeIn 0.2s ease-in-out;
}

@media (max-width: 768px) {
  .console-page {
    padding: 16px;
  }

  .console-header {
    flex-direction: column;
    gap: 12px;
  }
}
</style>
