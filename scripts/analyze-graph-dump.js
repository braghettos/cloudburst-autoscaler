#!/usr/bin/env node
/**
 * Analyze graph-e2e-all-providers.json to understand layout and edge overlap.
 * Run after integration test (before pod deletion). Usage:
 *   node scripts/analyze-graph-dump.js [path/to/graph-e2e-all-providers.json]
 *
 * Output: hierarchy, edge crossings, and suggested ordering for layout.
 */
const fs = require('fs')
const path = require('path')

function key(np) {
  return `${np.namespace ?? ''}/${np.name}`
}

function main() {
  const dumpPath =
    process.argv[2] ?? path.join(__dirname, '..', 'graph-e2e-all-providers.json')
  if (!fs.existsSync(dumpPath)) {
    console.error('Graph dump not found:', dumpPath)
    process.exit(1)
  }
  const dump = JSON.parse(fs.readFileSync(dumpPath, 'utf-8'))

  // CloudBurst filter (same as UI)
  const cbNodeNames = new Set(
    dump.nodes
      .filter((n) => n.cloudburstManaged || (n.name ?? '').startsWith('cb-'))
      .map((n) => n.name)
  )
  const cbNCs = dump.nodeclaims.filter((nc) => nc.name.startsWith('cb-'))
  const npKeysFromNCs = new Set(cbNCs.map((nc) => `${nc.namespace}/${nc.nodePoolRef}`))
  const npKeysFromCbNodes = new Set(
    dump.nodeclaims
      .filter((nc) => nc.nodeName && cbNodeNames.has(nc.nodeName))
      .map((nc) => `${nc.namespace}/${nc.nodePoolRef}`)
  )
  const allNpKeys = new Set([...npKeysFromNCs, ...npKeysFromCbNodes])
  const nodepools = dump.nodepools.filter((np) =>
    allNpKeys.has(`${np.namespace}/${np.name}`)
  )
  const nodeclaims = dump.nodeclaims.filter(
    (nc) =>
      nc.name.startsWith('cb-') ||
      (nc.nodeName != null && cbNodeNames.has(nc.nodeName))
  )
  const nodes = dump.nodes.filter((n) => cbNodeNames.has(n.name))
  const pods = dump.pods.filter((p) => p.nodeName && cbNodeNames.has(p.nodeName))

  console.log('=== Graph dump analysis (CloudBurst only) ===\n')
  console.log('Counts:', {
    nodepools: nodepools.length,
    nodeclaims: nodeclaims.length,
    nodes: nodes.length,
    pods: pods.length,
  })

  // Build maps
  const npToNcs = new Map()
  nodeclaims.forEach((nc) => {
    const k = `${nc.namespace}/${nc.nodePoolRef}`
    const list = npToNcs.get(k) ?? []
    list.push(nc)
    npToNcs.set(k, list)
  })
  const ncToNode = new Map()
  nodeclaims.forEach((nc) => {
    if (nc.nodeName) ncToNode.set(`${nc.namespace}/${nc.name}`, nc.nodeName)
  })
  const nodeToPods = new Map()
  pods.forEach((p) => {
    const list = nodeToPods.get(p.nodeName) ?? []
    list.push(p)
    nodeToPods.set(p.nodeName, list)
  })

  // Alphabetical order
  const sortedNPs = [...nodepools].sort((a, b) =>
    key(a).localeCompare(key(b))
  )
  const sortedNCs = [...nodeclaims].sort((a, b) =>
    key(a).localeCompare(key(b))
  )
  const sortedNodes = [...nodes].sort((a, b) =>
    a.name.localeCompare(b.name)
  )
  const sortedPods = [...pods].sort((a, b) =>
    key(a).localeCompare(key(b))
  )

  console.log('\n--- Alphabetical order (NP → NC → Node → Pod) ---')
  console.log('\nNodePools:', sortedNPs.map((np) => np.name).join(', '))
  console.log('\nNodeClaims:', sortedNCs.map((nc) => nc.name).join(', '))
  console.log('\nNodes:', sortedNodes.map((n) => n.name).join(', '))
  console.log('\nPods (first 20):', sortedPods.slice(0, 20).map((p) => `${p.namespace}/${p.name}`).join(', '))

  // Hierarchy
  console.log('\n--- Hierarchy (NP → NC → Node → Pod) ---')
  sortedNPs.forEach((np) => {
    const k = key(np)
    const ncs = npToNcs.get(k) ?? []
    const ncNames = ncs.sort((a, b) => a.name.localeCompare(b.name)).map((nc) => nc.name)
    console.log(`\n${np.name} (${ncNames.length} NCs)`)
    ncNames.forEach((ncName) => {
      const nc = nodeclaims.find((x) => x.namespace === np.namespace && x.name === ncName)
      const nodeName = nc?.nodeName ?? '(no node yet)'
      const podList = nodeName !== '(no node yet)' ? nodeToPods.get(nodeName) ?? [] : []
      const podNames = podList.sort((a, b) => key(a).localeCompare(key(b))).map((p) => `${p.namespace}/${p.name}`)
      console.log(`  ${ncName} → ${nodeName}`)
      podNames.forEach((pn) => console.log(`    └ ${pn}`))
    })
  })

  // Overlap analysis: multiple NCs per node
  const ncCountByNode = new Map()
  nodeclaims.forEach((nc) => {
    if (nc.nodeName) {
      const list = ncCountByNode.get(nc.nodeName) ?? []
      list.push(nc)
      ncCountByNode.set(nc.nodeName, list)
    }
  })
  const multiNcNodes = [...ncCountByNode.entries()].filter(([, list]) => list.length > 1)
  if (multiNcNodes.length > 0) {
    console.log('\n--- Multiple NodeClaims per Node (edge overlap risk) ---')
    multiNcNodes.forEach(([nodeName, ncs]) => {
      const npNames = [...new Set(ncs.map((nc) => nc.nodePoolRef))]
      console.log(`  ${nodeName}: ${ncs.length} NCs from NP(s) ${npNames.join(', ')}`)
      ncs.forEach((nc) => console.log(`    - ${nc.name}`))
    })
  }

  // NCs without node (orphans)
  const orphanNCs = nodeclaims.filter((nc) => !nc.nodeName)
  if (orphanNCs.length > 0) {
    console.log('\n--- NodeClaims without Node (Joining/Pending) ---')
    orphanNCs.forEach((nc) => console.log(`  ${nc.name} (${nc.phase ?? '?'})`))
  }

  console.log('\n--- Suggested layout order ---')
  console.log('1. NodePools: alphabetical by namespace/name')
  console.log('2. NodeClaims: alphabetical by namespace/name')
  console.log('3. Nodes: alphabetical by name')
  console.log('4. Pods: alphabetical by namespace/name (grouped under node)')
}

main()
