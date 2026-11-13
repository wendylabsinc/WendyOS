import { GoogleAnalytics } from '@next/third-parties/google';
import { isPreviewDeploy } from '@/lib/shared';

// Google Analytics 4 / Firebase Analytics — shares the same measurement ID as
// the marketing site (wendy.dev): the "marketing-website-wendy" Firebase web
// app (project cloud-c7e56), so docs traffic lands in the same GA4 property.
//
// @next/third-parties injects gtag.js via next/script and tracks App Router
// route changes automatically. Gated on the env var so local dev (where it is
// unset) sends nothing; CI sets it at build time for deployed docs.
const GA_MEASUREMENT_ID = process.env.NEXT_PUBLIC_GA_MEASUREMENT_ID;

export function Analytics() {
  // Branch previews are short-lived copies of the site. Crawlers find them and
  // keep requesting them after they are deleted, which filled the property with
  // preview page views and 404s. Only the published deploys report.
  if (!GA_MEASUREMENT_ID || isPreviewDeploy) {
    return null;
  }

  return <GoogleAnalytics gaId={GA_MEASUREMENT_ID} />;
}
