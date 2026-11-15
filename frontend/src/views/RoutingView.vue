<script setup>
import {onMounted, onUnmounted, ref} from "vue";
import {notify} from "@kyvg/vue3-notification";
import {isIP} from "is-ip";
import {apiWrapper} from "@/helpers/fetch-wrapper";
import {base64_url_encode} from "@/helpers/encoding";
import Modal from "@/components/Modal.vue";

const exits = ref([])
const lists = ref([])
const settings = ref({DnsForwarderEnabled: false})
const upstreams = ref('')
const peers = ref({})      // iface -> [{Identifier, DisplayName}]
const drafts = ref({})     // iface -> editable candidates
const dirty = ref({})      // iface -> unsaved candidate changes
const pick = ref({})       // iface -> picker text
const busy = ref(false)
let timer = null

const iface = (x) => encodeURIComponent(x.InterfaceIdentifier)

async function load() {
  try {
    const [e, l] = await Promise.all([apiWrapper.get('/routing/exits'), apiWrapper.get('/routing/lists')])
    exits.value = e || []
    lists.value = l || []
    for (const x of exits.value) {
      if (!dirty.value[x.InterfaceIdentifier]) drafts.value[x.InterfaceIdentifier] = (x.Candidates || []).map(c => ({...c}))
    }
  } catch (e) {
    notify({title: "Failed to load routing state", text: e.toString(), type: 'error'})
  }
}

async function loadOnce() {
  try {
    const s = await apiWrapper.get('/routing/settings')
    settings.value = s
    upstreams.value = (s.DnsUpstreams || []).join('\n')
  } catch (e) {
    notify({title: "Failed to load DNS settings", text: e.toString(), type: 'error'})
  }
  for (const x of exits.value) {
    try {
      peers.value[x.InterfaceIdentifier] = await apiWrapper.get(`/peer/iface/${base64_url_encode(x.InterfaceIdentifier)}/all`)
    } catch (e) {
      notify({title: `Failed to load peers of ${x.InterfaceIdentifier}`, text: e.toString(), type: 'error'})
    }
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

// --- exit nodes

function addCandidate(x) {
  const id = x.InterfaceIdentifier
  const text = (pick.value[id] || '').trim()
  const p = (peers.value[id] || []).find(p => p.Identifier === text || p.DisplayName === text)
  if (!p) return notify({title: "No peer matches " + text, type: 'warn'})
  if (drafts.value[id].some(c => c.PeerIdentifier === p.Identifier)) return
  drafts.value[id].push({PeerIdentifier: p.Identifier, DisplayName: p.DisplayName, EgressIp: ''})
  dirty.value[id] = true
  pick.value[id] = ''
}

// a peer that already carries 0.0.0.0/0 (detected): adopt it, the admin still has to enter its egress IP and save
function adoptDetected(x, d) {
  const id = x.InterfaceIdentifier
  if (drafts.value[id].some(c => c.PeerIdentifier === d.PeerIdentifier)) return
  drafts.value[id].push({PeerIdentifier: d.PeerIdentifier, DisplayName: d.DisplayName, EgressIp: ''})
  dirty.value[id] = true
}

function removeCandidate(x, i) {
  drafts.value[x.InterfaceIdentifier].splice(i, 1)
  dirty.value[x.InterfaceIdentifier] = true
}

const candidatesValid = (x) => drafts.value[x.InterfaceIdentifier].every(c => isIP(c.EgressIp.trim()))

const saveCandidates = (x) => run(`Exit candidates of ${x.InterfaceIdentifier} saved`, async () => {
  await apiWrapper.put(`/routing/exits/${iface(x)}/candidates`,
      drafts.value[x.InterfaceIdentifier].map(c => ({PeerIdentifier: c.PeerIdentifier, EgressIp: c.EgressIp.trim()})))
  dirty.value[x.InterfaceIdentifier] = false
})

function setActive(x, peerId) {
  if (peerId === x.ActivePeerIdentifier) return
  const c = x.Candidates.find(c => c.PeerIdentifier === peerId)
  const q = c ? `Route all exit traffic of ${x.InterfaceIdentifier} through ${c.DisplayName || peerId}?`
      : `Disable the exit node of ${x.InterfaceIdentifier}? Peers then leave through this server directly.`
  if (!window.confirm(q)) return
  run("Active exit node changed", () => apiWrapper.put(`/routing/exits/${iface(x)}/active`, {PeerIdentifier: peerId}))
}

// --- lists

const listUrl = (l) => `/routing/lists/${encodeURIComponent(l.Id)}`

const toggleList = (l) => run(`List ${l.Name} ${l.Enabled ? 'disabled' : 'enabled'}`, async () => {
  const full = await apiWrapper.get(listUrl(l))
  await apiWrapper.put(listUrl(l), {Name: full.Name, Kind: full.Kind, Enabled: !l.Enabled, Content: full.Content})
})

function deleteList(l) {
  if (window.confirm(`Delete list ${l.Name}?`)) run(`List ${l.Name} deleted`, () => apiWrapper.delete(listUrl(l)))
}

const edit = ref(null)       // list being edited
const editError = ref('')

async function openEdit(l) {
  try {
    edit.value = {...await apiWrapper.get(listUrl(l)), Id: l.Id}
    editError.value = ''
  } catch (e) {
    notify({title: "Failed to load list", text: e.toString(), type: 'error'})
  }
}

async function saveEdit() {
  busy.value = true
  try {
    const {Name, Kind, Enabled, Content} = edit.value
    await apiWrapper.put(listUrl(edit.value), {Name, Kind, Enabled, Content})
    edit.value = null
    notify({title: `List ${Name} saved`, type: 'success'})
  } catch (e) {
    editError.value = e.toString()
  } finally {
    busy.value = false
    await load()
  }
}

const add = ref(null)        // add-lists form
const addErrors = ref([])

function openAdd() {
  add.value = {Kind: 'blackhole', Enabled: true, Files: [], Name: '', Content: ''}
  addErrors.value = []
}

async function saveAdd() {
  const f = add.value
  const items = f.Files.length ? f.Files : [{name: f.Name.trim(), text: async () => f.Content}]
  busy.value = true
  addErrors.value = []
  const failed = []
  for (const file of items) {
    try {
      await apiWrapper.post('/routing/lists', {Name: file.name, Kind: f.Kind, Enabled: f.Enabled, Content: await file.text()})
    } catch (e) {
      failed.push(file)
      addErrors.value.push(`${file.name}: ${e.toString()}`)
    }
  }
  busy.value = false
  if (failed.length < items.length) notify({title: `${items.length - failed.length} list(s) added`, type: 'success'})
  if (failed.length === 0) add.value = null
  else if (f.Files.length) f.Files = failed // a retry only re-sends the files that failed
  await load()
}

// --- DNS forwarder

const saveSettings = () => run("DNS forwarder settings saved", async () => {
  await apiWrapper.put('/routing/settings', {
    DnsForwarderEnabled: settings.value.DnsForwarderEnabled,
    DnsUpstreams: upstreams.value.split('\n').map(s => s.trim()).filter(s => s),
  })
})

onMounted(async () => {
  await load()
  await loadOnce()
  timer = setInterval(load, 10000)
})
onUnmounted(() => clearInterval(timer))
</script>

<template>
  <div class="page-header">
    <h1>Routing</h1>
  </div>

  <p class="lead">
    Route peer internet traffic through an exit node, block or bypass destinations with lists, and forward peer DNS
    queries.
  </p>

  <h3 class="mt-4">Exit nodes</h3>
  <p v-if="exits.length === 0">No server interfaces found.</p>
  <div v-for="x in exits" :key="x.InterfaceIdentifier" class="card mb-3">
    <div class="card-header"><strong>{{ x.InterfaceIdentifier }}</strong></div>
    <div class="card-body">
      <div class="row">
        <div class="col-12 col-lg-4 mb-3">
          <h6>Active exit node</h6>
          <div class="form-check" v-for="c in [{PeerIdentifier: '', DisplayName: 'None (direct)'}, ...(x.Candidates || [])]"
               :key="c.PeerIdentifier" :class="{'text-success fw-bold': c.PeerIdentifier && c.PeerIdentifier === x.ActivePeerIdentifier}">
            <input class="form-check-input" type="radio" :id="`exit-${x.InterfaceIdentifier}-${c.PeerIdentifier}`"
                   :name="`exit-${x.InterfaceIdentifier}`" :disabled="busy"
                   :checked="(x.ActivePeerIdentifier || '') === c.PeerIdentifier" @click.prevent="setActive(x, c.PeerIdentifier)">
            <label class="form-check-label" :for="`exit-${x.InterfaceIdentifier}-${c.PeerIdentifier}`">
              {{ c.DisplayName || c.PeerIdentifier }}
              <small v-if="c.EgressIp" class="text-muted">({{ c.EgressIp }})</small>
              <span v-if="c.PeerIdentifier && c.PeerIdentifier === x.ActivePeerIdentifier" class="badge bg-success ms-1">active</span>
            </label>
          </div>
        </div>

        <div class="col-12 col-lg-8">
          <div v-if="x.Detected?.length" class="alert alert-warning py-2">
            <strong>Detected default routes:</strong> these peers carry <code>0.0.0.0/0</code> or <code>::/0</code>
            but are no exit candidates.
            <span v-if="!x.Candidates?.length">Until this interface has exit candidates, such a route sends <em>all</em>
              of a standalone server's own traffic through the peer. HA nodes ignore it.</span>
            <span v-else>They are ignored while this interface has exit candidates.</span>
            Add them as candidates, enter their egress IP and save.
            <div v-for="d in x.Detected" :key="d.PeerIdentifier" class="mt-1">
              {{ d.DisplayName || d.PeerIdentifier }}
              <button class="btn btn-sm btn-outline-dark ms-2" type="button" :disabled="busy"
                      @click="adoptDetected(x, d)">Add as candidate</button>
            </div>
          </div>
          <h6>Candidates</h6>
          <table class="table table-sm align-middle" v-if="drafts[x.InterfaceIdentifier]?.length">
            <thead>
            <tr>
              <th scope="col">Peer</th>
              <th scope="col">Egress IP</th>
              <th scope="col" class="text-end"></th>
            </tr>
            </thead>
            <tbody>
            <tr v-for="(c, i) in drafts[x.InterfaceIdentifier]" :key="c.PeerIdentifier">
              <td>{{ c.DisplayName || c.PeerIdentifier }} <br><small class="text-muted text-break">{{ c.PeerIdentifier }}</small></td>
              <td>
                <input v-model="c.EgressIp" class="form-control form-control-sm" :class="{'is-invalid': !isIP(c.EgressIp.trim())}"
                       placeholder="public IP of the exit host" type="text" required
                       @input="dirty[x.InterfaceIdentifier] = true">
              </td>
              <td class="text-end">
                <button class="btn btn-sm btn-outline-danger" :disabled="busy" @click="removeCandidate(x, i)" title="Remove candidate">
                  <i class="fa-solid fa-trash"></i>
                </button>
              </td>
            </tr>
            </tbody>
          </table>
          <p v-else class="text-muted">No candidates yet.</p>

          <form class="row g-2" @submit.prevent="addCandidate(x)">
            <div class="col">
              <input v-model="pick[x.InterfaceIdentifier]" class="form-control form-control-sm" :list="`peers-${x.InterfaceIdentifier}`"
                     placeholder="search peers by name or key" type="text">
              <datalist :id="`peers-${x.InterfaceIdentifier}`">
                <option v-for="p in peers[x.InterfaceIdentifier] || []" :key="p.Identifier" :value="p.Identifier">{{ p.DisplayName }}</option>
              </datalist>
            </div>
            <div class="col-auto">
              <button class="btn btn-sm btn-secondary" type="submit"><i class="fa-solid fa-plus"></i> Add</button>
            </div>
            <div class="col-auto">
              <button class="btn btn-sm btn-primary" type="button" :disabled="busy || !dirty[x.InterfaceIdentifier] || !candidatesValid(x)"
                      @click="saveCandidates(x)">Save candidates</button>
            </div>
          </form>
        </div>
      </div>
    </div>
  </div>

  <h3 class="mt-4">Lists</h3>
  <p class="text-muted small">
    One IP, CIDR or domain per line (<code>#</code> comments, hosts-file lines accepted). <strong>Blackhole</strong> drops
    matching traffic, <strong>bypass</strong> sends it directly instead of through the exit node.
  </p>
  <button class="btn btn-primary btn-sm mb-2" :disabled="busy" @click="openAdd"><i class="fa-solid fa-plus"></i> Add lists</button>
  <div class="table-responsive">
    <p v-if="lists.length === 0">No lists yet.</p>
    <table v-else class="table table-sm align-middle">
      <thead>
      <tr>
        <th scope="col">Name</th>
        <th scope="col">Kind</th>
        <th scope="col">Enabled</th>
        <th scope="col" class="text-center">Entries</th>
        <th scope="col">Updated</th>
        <th scope="col" class="text-end">Actions</th>
      </tr>
      </thead>
      <tbody>
      <tr v-for="l in lists" :key="l.Id">
        <td>{{ l.Name }}</td>
        <td><span class="badge" :class="l.Kind === 'blackhole' ? 'bg-dark' : 'bg-info'">{{ l.Kind }}</span></td>
        <td>
          <div class="form-check form-switch">
            <input class="form-check-input" type="checkbox" :checked="l.Enabled" :disabled="busy" @click.prevent="toggleList(l)">
          </div>
        </td>
        <td class="text-center">{{ l.EntryCount }}</td>
        <td><small>{{ l.UpdatedAt ? new Date(l.UpdatedAt).toLocaleString() : '' }} {{ l.UpdatedBy ? `(${l.UpdatedBy})` : '' }}</small></td>
        <td class="text-end text-nowrap">
          <button class="btn btn-sm btn-secondary me-1" :disabled="busy" @click="openEdit(l)" title="Edit list">
            <i class="fa-solid fa-pen"></i>
          </button>
          <button class="btn btn-sm btn-outline-danger" :disabled="busy" @click="deleteList(l)" title="Delete list">
            <i class="fa-solid fa-trash"></i>
          </button>
        </td>
      </tr>
      </tbody>
    </table>
  </div>

  <h3 class="mt-4">DNS forwarder</h3>
  <p class="text-muted small">
    Answers peer DNS queries on the tunnel address and learns the IPs of listed domains. Peers must use the interface's
    tunnel address as their DNS server. A Pi-hole can be a VPN peer and serve as upstream through its tunnel IP.
  </p>
  <form @submit.prevent="saveSettings">
    <div class="form-check form-switch mb-2">
      <input id="dnsEnabled" v-model="settings.DnsForwarderEnabled" class="form-check-input" type="checkbox">
      <label class="form-check-label" for="dnsEnabled">Enable DNS forwarder</label>
    </div>
    <div class="col-12 col-md-6 mb-2">
      <label class="form-label" for="dnsUpstreams">Upstream servers (one <code>host[:port]</code> per line)</label>
      <textarea id="dnsUpstreams" v-model="upstreams" class="form-control" rows="3" placeholder="10.11.12.5&#10;1.1.1.1:53"></textarea>
    </div>
    <button class="btn btn-primary" :disabled="busy" type="submit">Save</button>
  </form>

  <Modal title="Add lists" :visible="!!add" @close="add = null">
    <template v-if="add">
      <div class="row g-2 mb-3">
        <div class="col-6">
          <label class="form-label" for="addKind">Kind</label>
          <select id="addKind" v-model="add.Kind" class="form-select">
            <option value="blackhole">blackhole</option>
            <option value="bypass">bypass</option>
          </select>
        </div>
        <div class="col-6 d-flex align-items-end">
          <div class="form-check">
            <input id="addEnabled" v-model="add.Enabled" class="form-check-input" type="checkbox">
            <label class="form-check-label" for="addEnabled">Enabled</label>
          </div>
        </div>
      </div>
      <label class="form-label" for="addFiles">Upload files (one list per file, named after the file)</label>
      <input id="addFiles" class="form-control mb-3" type="file" multiple accept=".txt,.list,.hosts,text/plain"
             @change="add.Files = [...$event.target.files]">
      <template v-if="add.Files.length === 0">
        <p class="text-muted small mb-1">or paste a single list:</p>
        <input v-model="add.Name" class="form-control mb-2" placeholder="Name" type="text">
        <textarea v-model="add.Content" class="form-control font-monospace" rows="8" placeholder="example.com&#10;10.0.0.0/8"></textarea>
      </template>
      <div v-for="err in addErrors" :key="err" class="alert alert-danger mt-2 mb-0 py-2" style="white-space: pre-wrap">{{ err }}</div>
    </template>
    <template #footer>
      <button class="btn btn-primary" :disabled="busy || !add || (add.Files.length === 0 && !add.Name.trim())" @click="saveAdd">Add</button>
      <button class="btn btn-secondary" @click="add = null">Cancel</button>
    </template>
  </Modal>

  <Modal title="Edit list" :visible="!!edit" @close="edit = null">
    <template v-if="edit">
      <div class="row g-2 mb-2">
        <div class="col-12 col-md-6">
          <label class="form-label" for="editName">Name</label>
          <input id="editName" v-model="edit.Name" class="form-control" type="text">
        </div>
        <div class="col-6 col-md-3">
          <label class="form-label" for="editKind">Kind</label>
          <select id="editKind" v-model="edit.Kind" class="form-select">
            <option value="blackhole">blackhole</option>
            <option value="bypass">bypass</option>
          </select>
        </div>
        <div class="col-6 col-md-3 d-flex align-items-end">
          <div class="form-check">
            <input id="editEnabled" v-model="edit.Enabled" class="form-check-input" type="checkbox">
            <label class="form-check-label" for="editEnabled">Enabled</label>
          </div>
        </div>
      </div>
      <textarea v-model="edit.Content" class="form-control font-monospace" rows="14"></textarea>
      <div v-if="editError" class="alert alert-danger mt-2 mb-0 py-2" style="white-space: pre-wrap">{{ editError }}</div>
    </template>
    <template #footer>
      <button class="btn btn-primary" :disabled="busy || !edit?.Name.trim()" @click="saveEdit">Save</button>
      <button class="btn btn-secondary" @click="edit = null">Cancel</button>
    </template>
  </Modal>
</template>
