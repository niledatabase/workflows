const fs = require('node:fs')
const path = require('node:path')

const repository = 'niledatabase/workflows'
const releaseWorkflow = `${repository}/.github/workflows/buildkit-cache-mounts-release.yaml`
const goArchitectures = { x64: 'amd64', arm64: 'arm64' }

/**
 * Downloads the buildkit-cache-mounts binary from the release for binaryCommit, verifies its attestation, and returns the path of the binary.
 * A later call in the same job returns the binary that is already downloaded.
 * It rejects if the download or the verification fails.
 */
module.exports.downloadBinary = async ({ exec }, binaryCommit) => {
  const directory = path.join(process.env.RUNNER_TEMP, 'buildkit-cache-mounts', binaryCommit)
  const binary = path.join(directory, 'buildkit-cache-mounts')
  if (!fs.existsSync(binary)) {
    const asset = `buildkit-cache-mounts-linux-${goArchitectures[process.arch]}`
    const downloadDirectory = fs.mkdtempSync(path.join(process.env.RUNNER_TEMP, 'buildkit-cache-mounts-'))
    const download = path.join(downloadDirectory, asset)
    await exec.exec('gh', [
      'release', 'download', `buildkit-cache-mounts-${binaryCommit}`,
      '--repo', repository,
      '--pattern', asset,
      '--dir', downloadDirectory,
    ])
    await exec.exec('gh', [
      'attestation', 'verify', download,
      '--repo', repository,
      '--signer-workflow', releaseWorkflow,
      '--source-digest', binaryCommit,
      '--source-ref', 'refs/heads/main',
      '--deny-self-hosted-runners',
    ])
    fs.chmodSync(download, 0o755)
    fs.mkdirSync(directory, { recursive: true })
    fs.renameSync(download, binary)
  }
  return binary
}
