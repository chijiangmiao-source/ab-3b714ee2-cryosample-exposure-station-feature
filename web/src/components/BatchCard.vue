<script setup>
import { computed } from 'vue'
import {
  canReturn,
  canTakeout,
  eventTypeLabel,
  remainingSeconds,
  stateLabel,
  statusLabel
} from '../lib/derive'

const props = defineProps({
  batch: { type: Object, required: true },
  busy: { type: Boolean, default: false }
})

const emit = defineEmits(['takeout', 'return'])

const remaining = computed(() => remainingSeconds(props.batch))
const takeoutOk = computed(() => canTakeout(props.batch))
const returnOk = computed(() => canReturn(props.batch))
</script>

<template>
  <section class="card" data-test="batch-card">
    <header class="card-head">
      <h2 data-test="card-barcode">{{ batch.barcode }}</h2>
      <span class="badge" :class="batch.state" data-test="state-badge">{{ stateLabel(batch) }}</span>
      <span class="badge" :class="batch.status" data-test="status-badge">{{ statusLabel(batch) }}</span>
    </header>

    <dl class="grid">
      <div>
        <dt>允许暴露</dt>
        <dd data-test="allowed">{{ batch.allowedSeconds }} 秒</dd>
      </div>
      <div>
        <dt>累计暴露</dt>
        <dd data-test="accumulated">{{ batch.accumulatedSeconds }} 秒</dd>
      </div>
      <div>
        <dt>剩余额度</dt>
        <dd data-test="remaining">{{ remaining }} 秒</dd>
      </div>
      <div>
        <dt>创建时刻</dt>
        <dd data-test="created-at">{{ batch.createdAt }}</dd>
      </div>
      <div class="wide">
        <dt>最后事件</dt>
        <dd data-test="last-event">
          <template v-if="batch.lastEvent">
            {{ eventTypeLabel(batch.lastEvent.type) }} @ {{ batch.lastEvent.at }}
            <template v-if="batch.lastEvent.deltaSeconds != null">
              （本次 +{{ batch.lastEvent.deltaSeconds }} 秒）
            </template>
          </template>
          <template v-else>尚无事件</template>
        </dd>
      </div>
      <div class="wide">
        <dt>结论</dt>
        <dd data-test="conclusion" :class="batch.status">
          {{ statusLabel(batch) }}<template v-if="batch.status === 'scrapped'">（累计超限，永久报废，不得再取出）</template>
        </dd>
      </div>
    </dl>

    <div class="actions">
      <button
        type="button"
        class="takeout"
        data-test="takeout-btn"
        :disabled="busy || !takeoutOk"
        @click="emit('takeout')"
      >取出</button>
      <button
        type="button"
        class="return"
        data-test="return-btn"
        :disabled="busy || !returnOk"
        @click="emit('return')"
      >归还</button>
    </div>
  </section>
</template>
