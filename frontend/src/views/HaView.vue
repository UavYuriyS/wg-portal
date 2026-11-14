<script setup>
import {computed, onMounted, onUnmounted, ref} from "vue";
import {notify} from "@kyvg/vue3-notification";
import {apiWrapper} from "@/helpers/fetch-wrapper";

const status = ref({Role: '', Nodes: [], JoinTokens: []})
const newToken = ref('')
const tokenForm = ref({ValidHours: 24, Uses: 1})
const busy = ref(false)
let timer = null

const isOrchestrator = computed(() => status.value.Role === 'orchestrator')
const orchestratorUrl = computed(() => window.location.origin + window.location.pathname.replace(/\/app\/?$/, ''))

async function load() {
  try {
    status.value = await apiWrapper.get('/ha/status')
  } catch (e) {
    notify({title: "Failed to load HA status", text: e.toString(), type: 'error'})
  }
}

async function run(title, fn) {
  busy.value = true
  try {
    await fn()
    notify({title, type: 'success'})
  } catch (e) {
    notify({title: title + " failed", text: e.toString(), type: 'error'})
  } finally {
    busy.value = false
    await load()
  }
}

const promote = (node) => run(`Promoted ${node.NodeId}`,
    () => apiWrapper.post(`/ha/promote/${encodeURIComponent(node.NodeId)}`))
const removeNode = (node) => run(`Removed ${node.NodeId}`,
    () => apiWrapper.delete(`/ha/nodes/${encodeURIComponent(node.NodeId)}`))
const revokeToken = (t) => run("Join token revoked", () => apiWrapper.delete(`/ha/join-tokens/${t.Id}`))
const createToken = () => run("Join token created", async () => {
  const resp = await apiWrapper.post('/ha/join-tokens', {
    ValidHours: Number(tokenForm.value.ValidHours), Uses: Number(tokenForm.value.Uses),
  })
  newToken.value = resp.Token
})

const joinCommand = computed(() =>
    `WG_PORTAL_HA_ORCHESTRATOR_URL=${orchestratorUrl.value} WG_PORTAL_HA_JOIN_TOKEN=${newToken.value} docker compose -f docker-compose.ha-node.yml up -d`)

function age(ts) {
  if (!ts) return 'never'
  const s = Math.round((Date.now() - new Date(ts).getTime()) / 1000)
  return s < 120 ? `${s}s ago` : `${Math.round(s / 60)}m ago`
}

onMounted(async () => {
  await load()
  timer = setInterval(load, 10000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div class="page-header">
    <h1>High Availability</h1>
  </div>

  <p class="lead" v-if="!isOrchestrator">
    This instance is not an HA orchestrator (role: {{ status.Role || 'unknown' }}). Set <code>ha.role: orchestrator</code> to manage a cluster.
  </p>

  <template v-if="isOrchestrator">
    <p class="lead">
      WireGuard nodes and the active node that the VPN DNS name points to. The orchestrator probes every node through
      the tunnel and promotes a healthy standby when the active node fails.
    </p>

    <h3 class="mt-4">Nodes</h3>
    <div class="table-responsive">
      <p v-if="status.Nodes.length === 0">No nodes registered yet. Create a join token below and start a node with it.</p>
      <table v-else class="table table-sm align-middle">
        <thead>
        <tr>
          <th scope="col">Node</th>
          <th scope="col">Public IP</th>
          <th scope="col" class="text-center">Priority</th>
          <th scope="col">Probe</th>
          <th scope="col">Heartbeat</th>
          <th scope="col" class="text-end">Actions</th>
        </tr>
        </thead>
        <tbody>
        <tr v-for="node in status.Nodes" :key="node.NodeId">
          <td>
            {{ node.NodeId }}
            <span v-if="node.IsActive" class="badge bg-success ms-1">active</span>
          </td>
          <td><code>{{ node.PublicIp }}</code></td>
          <td class="text-center">{{ node.Priority }}</td>
          <td>
            <span class="badge" :class="node.LastProbeOk ? 'bg-success' : 'bg-danger'">
              {{ node.LastProbeOk ? 'ok' : (node.LastProbeAt ? `failing (${node.ConsecutiveFails})` : 'pending') }}
            </span>
            <small class="text-muted ms-1">{{ node.LastProbeResult }} · {{ age(node.LastProbeAt) }}</small>
          </td>
          <td>{{ age(node.LastHeartbeat) }}</td>
          <td class="text-end text-nowrap">
            <button class="btn btn-sm btn-primary me-1" :disabled="busy || node.IsActive" @click="promote(node)"
                    title="Make this the active node and point DNS at it">
              <i class="fa-solid fa-arrow-up"></i> Promote
            </button>
            <button class="btn btn-sm btn-outline-danger" :disabled="busy || node.IsActive" @click="removeNode(node)"
                    title="Remove this standby from the cluster">
              <i class="fa-solid fa-trash"></i>
            </button>
          </td>
        </tr>
        </tbody>
      </table>
    </div>
    <p class="text-muted small" v-if="status.ChangedBy">
      Last change: {{ new Date(status.ChangedAt).toLocaleString() }} by {{ status.ChangedBy }}
    </p>

    <template v-if="status.ExitCheck?.ExpectedIp">
      <h3 class="mt-4">Exit check</h3>
      <p class="mb-1">
        Expected egress IP <code>{{ status.ExitCheck.ExpectedIp }}</code>
        <span class="badge ms-1" :class="status.ExitCheck.Ok ? 'bg-success' : (status.ExitCheck.At ? 'bg-danger' : 'bg-secondary')">
          {{ status.ExitCheck.Ok ? 'ok' : (status.ExitCheck.At ? 'failed' : 'pending') }}
        </span>
        <small class="text-muted ms-1">{{ status.ExitCheck.Result }} · {{ age(status.ExitCheck.At) }}</small>
      </p>
      <p class="text-muted small">
        Informational only, does not trigger failover: traffic from the active node is sent through the active exit
        node and its egress IP is compared. Exit nodes are managed under
        <RouterLink :to="{ name: 'routing' }">Routing</RouterLink>.
      </p>
    </template>

    <h3 class="mt-4">Join tokens</h3>
    <form class="row g-2 align-items-end" @submit.prevent="createToken">
      <div class="col-6 col-md-2">
        <label class="form-label" for="haTokenHours">Valid (hours)</label>
        <input id="haTokenHours" v-model="tokenForm.ValidHours" class="form-control" min="1" type="number">
      </div>
      <div class="col-6 col-md-2">
        <label class="form-label" for="haTokenUses">Uses</label>
        <input id="haTokenUses" v-model="tokenForm.Uses" class="form-control" min="1" type="number">
      </div>
      <div class="col-auto">
        <button class="btn btn-primary" :disabled="busy" type="submit"><i class="fa-solid fa-plus"></i> Create join token</button>
      </div>
    </form>

    <div v-if="newToken" class="alert alert-info mt-3" role="status">
      <p class="mb-1"><strong>Copy this token now, it is shown only once:</strong></p>
      <code class="user-select-all">{{ newToken }}</code>
      <p class="mt-2 mb-1">Start a node with:</p>
      <code class="user-select-all">{{ joinCommand }}</code>
    </div>

    <div class="table-responsive mt-3" v-if="status.JoinTokens.length > 0">
      <table class="table table-sm align-middle">
        <thead>
        <tr>
          <th scope="col">Id</th>
          <th scope="col">Created</th>
          <th scope="col">Expires</th>
          <th scope="col" class="text-center">Uses left</th>
          <th scope="col" class="text-end">Actions</th>
        </tr>
        </thead>
        <tbody>
        <tr v-for="t in status.JoinTokens" :key="t.Id">
          <td><code>{{ t.Id }}</code></td>
          <td>{{ new Date(t.CreatedAt).toLocaleString() }} ({{ t.CreatedBy }})</td>
          <td>{{ new Date(t.ExpiresAt).toLocaleString() }}</td>
          <td class="text-center">{{ t.UsesLeft }}</td>
          <td class="text-end">
            <button class="btn btn-sm btn-outline-danger" :disabled="busy" @click="revokeToken(t)" title="Revoke token">
              <i class="fa-solid fa-trash"></i>
            </button>
          </td>
        </tr>
        </tbody>
      </table>
    </div>
  </template>
</template>
