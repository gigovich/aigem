import { expect, test } from 'vitest'
import { IMAGE_LIMIT, readImage } from './image'

// jsdom has no canvas, so a small file goes through untouched: the base64 of
// its bytes, its type, its size.
test('a small image is sent as it is', async () => {
  const bytes = new Uint8Array([137, 80, 78, 71, 13, 10, 26, 10])
  const file = new File([bytes], 'shot.png', { type: 'image/png' })
  const out = await readImage(file)
  expect(out).toEqual({
    name: 'shot.png',
    media_type: 'image/png',
    data: 'iVBORw0KGgo=',
    bytes: 8,
  })
})

test('a file that is not an image is refused', async () => {
  const file = new File(['hello'], 'notes.txt', { type: 'text/plain' })
  await expect(readImage(file)).rejects.toThrow('not an image')
})

// Without a canvas nothing can be scaled, so a large file is refused rather
// than sent past the frame cap.
test('a large image is refused where it cannot be scaled', async () => {
  const file = new File([new Uint8Array(IMAGE_LIMIT + 1)], 'big.png', { type: 'image/png' })
  await expect(readImage(file)).rejects.toThrow('too large')
})
