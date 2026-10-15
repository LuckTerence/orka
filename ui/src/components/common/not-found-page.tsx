import { Link, useLocation } from '@tanstack/react-router'
import { Compass } from 'lucide-react'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { EmptyState } from '@/components/ui/empty-state'
import { PageHeader } from '@/components/layout/page-header'

/** Shown inside the dashboard layout for any address no route matches. */
export function NotFoundPage() {
  const { pathname } = useLocation()
  return (
    <div className="space-y-4">
      <PageHeader title="Page not found" />
      <Card>
        <CardContent>
          <EmptyState
            icon={Compass}
            headline={`Nothing lives at ${pathname}`}
            hint="The address may be mistyped, or the page may have moved."
            action={
              <Button asChild>
                <Link to="/">Go to Dashboard</Link>
              </Button>
            }
          />
        </CardContent>
      </Card>
    </div>
  )
}
