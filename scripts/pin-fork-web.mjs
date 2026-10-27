// Re-attest the artifact built in CI from an explicitly checked-out fork SHA.
// Never keep the upstream download URL emitted by older pack-web-artifact tools.
import { readdir, readFile, mkdir, copyFile, writeFile } from 'node:fs/promises'
import { join, basename } from 'node:path'
import { createHash } from 'node:crypto'
const [source, output, commit] = process.argv.slice(2)
if (!source || !output || !/^[0-9a-f]{40}$/.test(commit ?? '')) throw new Error('usage: pin-fork-web.mjs <artifact-dir> <output-dir> <fork-commit>')
const manifests = (await readdir(source)).filter(p => p.endsWith('.tgz.json'))
if (manifests.length !== 1) throw new Error('expected one locally built browser manifest')
const manifest = JSON.parse(await readFile(join(source, manifests[0]), 'utf8'))
if (manifest.source.commit !== commit || manifest.source.dirty !== false) throw new Error('browser artifact does not match clean pinned fork commit')
const archive = manifest.archive.file
if (basename(archive) !== archive || !archive.endsWith('.tgz')) throw new Error('unsafe archive name')
const bytes = await readFile(join(source, archive))
if (bytes.length !== manifest.archive.size || createHash('sha256').update(bytes).digest('hex') !== manifest.archive.sha256) throw new Error('browser archive checksum mismatch')
manifest.source.repository = 'https://github.com/maxzhao/zennotes'
delete manifest.archive.url
await mkdir(output, { recursive: true })
await copyFile(join(source, archive), join(output, archive))
await writeFile(join(output, 'manifest.json'), JSON.stringify(manifest, null, 2) + '\n')
