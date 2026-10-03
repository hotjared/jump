import { useCallback, useEffect, useState } from 'react'
import type { NoticeMessage } from './Notice'

export function useRemoteNotice() {
  const [notice, setNotice] = useState<NoticeMessage | null>(null)
  const notify = useCallback((text: string, severity: NoticeMessage['severity'] = 'info') => setNotice({ text, severity }), [])
  const dismiss = useCallback(() => setNotice(null), [])
  return { notice, notify, dismiss }
}

// Keep action feedback outside the measured viewport's document flow.
// One slot replaces repeated notices, and each new notice restarts the timer.
export default function RemoteNotice({ notice, dismiss }: { notice: NoticeMessage | null; dismiss: () => void }) {
  useEffect(() => {
    if (!notice) return
    const timer = setTimeout(dismiss, notice.severity === 'error' || notice.severity === 'warning' ? 6000 : 3000)
    return () => clearTimeout(timer)
  }, [notice, dismiss])
  if (!notice) return null
  return <div className={`remote-notice remote-notice-${notice.severity}`} role={notice.severity === 'error' ? 'alert' : 'status'}>
    {notice.text}<button type="button" className="text-button" aria-label="Dismiss session notification" onClick={dismiss}>×</button>
  </div>
}
