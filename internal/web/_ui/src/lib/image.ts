/**
 * An image for the composer: base64, inside the daemon's frame cap.
 *
 * A screenshot is several megabytes and the run socket takes one megabyte per
 * frame, so a large image is drawn onto a canvas at most 1568 px on its long
 * edge and re-encoded as JPEG. A browser without a canvas - or a test runner -
 * cannot do that, and refuses the image rather than sending one that would end
 * the socket.
 */

export type Attachment = { name: string; media_type: string; data: string; bytes: number }

export const IMAGE_LIMIT = 700 * 1024
export const WIRE_LIMIT = 700 * 1024
const LONG_EDGE = 1568
const TYPES = new Set(['image/png', 'image/jpeg', 'image/webp', 'image/gif'])

export async function readImage(file: Blob & { name?: string }): Promise<Attachment> {
  if (!TYPES.has(file.type)) throw new Error(`${file.name ?? 'the clipboard'} is not an image`)
  const name = file.name ?? 'pasted image'
  if (file.size <= IMAGE_LIMIT) {
    return { name, media_type: file.type, data: await base64(file), bytes: file.size }
  }
  const scaled = await shrink(file)
  if (!scaled) throw new Error(`${name} is too large to send and cannot be scaled here`)
  return { name, media_type: 'image/jpeg', data: await base64(scaled), bytes: scaled.size }
}

async function shrink(file: Blob): Promise<Blob | null> {
  if (typeof createImageBitmap !== 'function') return null
  const canvas = document.createElement('canvas')
  const ctx = canvas.getContext('2d')
  if (!ctx) return null
  const bitmap = await createImageBitmap(file)
  const scale = Math.min(1, LONG_EDGE / Math.max(bitmap.width, bitmap.height))
  canvas.width = Math.round(bitmap.width * scale)
  canvas.height = Math.round(bitmap.height * scale)
  ctx.drawImage(bitmap, 0, 0, canvas.width, canvas.height)
  bitmap.close()
  const encode = (quality: number) =>
    new Promise<Blob | null>((r) => canvas.toBlob(r, 'image/jpeg', quality))
  const first = await encode(0.85)
  if (first && first.size <= IMAGE_LIMIT) return first
  const second = await encode(0.6)
  return second && second.size <= IMAGE_LIMIT ? second : null
}

function base64(blob: Blob): Promise<string> {
  return new Promise((resolve, reject) => {
    const reader = new FileReader()
    reader.onerror = () => reject(reader.error ?? new Error('could not read the image'))
    reader.onload = () => {
      const result = reader.result
      resolve(typeof result === 'string' ? (result.split(',', 2)[1] ?? '') : '')
    }
    reader.readAsDataURL(blob)
  })
}
