import { useEffect, useState } from 'react'

export default function CopyButton({ value, label }: { value: string; label: string }) {
  const [feedback, setFeedback] = useState('')
  useEffect(() => {
    setFeedback('')
  }, [value])
  useEffect(() => {
    if (!feedback) return
    const timer = setTimeout(() => setFeedback(''), 3000)
    return () => clearTimeout(timer)
  }, [feedback])
  async function copy() {
    try { await navigator.clipboard.writeText(value); setFeedback('Copied') }
    catch { setFeedback('Copy failed') }
  }
  return <button type="button" className="button copy-button" aria-label={label} title={label} onClick={() => void copy()}>
    <span role="status">{feedback || 'Copy'}</span>
  </button>
}
