import { useEffect } from 'react'

export type NoticeMessage = { text: string; severity: 'success' | 'info' | 'warning' | 'error' }

export default function Notice({ notice, dismiss }: { notice: NoticeMessage | null; dismiss: () => void }) {
  useEffect(() => {
    if (!notice || (notice.severity !== 'success' && notice.severity !== 'info')) return
    const timer = setTimeout(dismiss, 5000)
    return () => clearTimeout(timer)
  }, [notice, dismiss])

  if (!notice) return null
  return <div className={notice.severity === 'success' || notice.severity === 'info' ? 'quick-notice' : 'error'} role={notice.severity === 'error' || notice.severity === 'warning' ? 'alert' : 'status'}>
    {notice.text}<button className="text-button" aria-label="Dismiss notification" onClick={dismiss}>×</button>
  </div>
}
