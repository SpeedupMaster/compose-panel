import test from 'node:test'
import assert from 'node:assert/strict'
import { createEscapeStack } from '../src/components/ui/escapeStack.ts'

test('createEscapeStack dispatches only the topmost layer in LIFO order', () => {
  const stack = createEscapeStack()
  const calls = []

  assert.equal(stack.handleKey('Escape'), false)

  const popSidebar = stack.push(() => calls.push('sidebar'))
  const popModal = stack.push(() => calls.push('modal'))
  const popDrawer = stack.push(() => calls.push('drawer'))
  assert.equal(stack.size(), 3)

  // Non-Escape keys are ignored.
  assert.equal(stack.handleKey('Enter'), false)
  assert.deepEqual(calls, [])

  // Topmost layer (drawer) handles Escape.
  assert.equal(stack.handleKey('Escape'), true)
  assert.deepEqual(calls, ['drawer'])

  popDrawer()
  // Double unsubscribe is a safe no-op.
  popDrawer()
  assert.equal(stack.size(), 2)

  assert.equal(stack.handleKey('Escape'), true)
  assert.deepEqual(calls, ['drawer', 'modal'])

  // Removing a non-top layer leaves the rest intact.
  popSidebar()
  assert.equal(stack.size(), 1)
  assert.equal(stack.handleKey('Escape'), true)
  assert.deepEqual(calls, ['drawer', 'modal', 'modal'])

  popModal()
  assert.equal(stack.size(), 0)
  assert.equal(stack.handleKey('Escape'), false)
})
