import { getMDXComponents } from '@/components/mdx';
import { source } from '@/lib/source';
import { ogImage, withBasePath } from '@/lib/shared';
import { DocsBody, DocsDescription, DocsPage, DocsTitle } from 'fumadocs-ui/layouts/docs/page';
import { createRelativeLink } from 'fumadocs-ui/mdx';
import type { Metadata } from 'next';
import { notFound, permanentRedirect } from 'next/navigation';

type PageParams = {
  slug?: string[];
};

type PageProps = {
  params: Promise<PageParams>;
};

// OpenGraph cards keyed by page slug.
const pageOgImages: Record<string, string> = {
  'installation/wendyos-nvidia-jetson-agx-thor': '/images/opengraph-thor.png',
  'integrations/ros2': '/images/opengraph-ros2.png',
};

const tutorialOgImages: Record<string, string> = {
  'guides/camera-exposure': '/images/og-tutorial-camera-exposure.png',
  'guides/fleet-deployment': '/images/og-tutorial-fleet-deployment.png',
  'guides/tutorials/python/amd-rocm-pytorch': '/images/og-tutorial-amd-rocm-pytorch.png',
  'guides/tutorials/python/robot-camera-opencv': '/images/og-tutorial-robot-camera-opencv.png',
  'guides/tutorials/python/g1-mujoco-simulation': '/images/og-tutorial-g1-mujoco-simulation.png',
  'guides/tutorials/python/raw-camera-frames': '/images/og-tutorial-raw-camera-frames.png',
  'guides/tutorials/python/cloud-udp-service': '/images/og-tutorial-cloud-udp-service.png',
};

const legacyRedirects: Record<string, string> = {
  'installation/wendyos-nvidia-jetson': '/installation/wendyos-nvidia-jetson-orin-nano/',
  'installation/wendy-agent-linux': '/installation/linux/',
  'installation/ubuntu': '/installation/linux/',
  'guides/tutorials/camera-exposure': '/guides/camera-exposure/',
  'guides/tutorials/fleet-deployment': '/guides/fleet-deployment/',
};

function getLegacyRedirect(slug?: string[]) {
  return legacyRedirects[slug?.join('/') ?? ''];
}

export default async function Page(props: PageProps) {
  const params = await props.params;
  const redirect = getLegacyRedirect(params.slug);
  if (redirect) {
    permanentRedirect(redirect);
  }

  const page = source.getPage(params.slug);
  if (!page) notFound();

  const MDX = page.data.body;

  return (
    <DocsPage toc={page.data.toc} full={page.data.full}>
      <DocsTitle>{page.data.title}</DocsTitle>
      <DocsDescription>{page.data.description}</DocsDescription>
      <DocsBody>
        <MDX
          components={getMDXComponents({
            a: createRelativeLink(source, page),
          })}
        />
      </DocsBody>
    </DocsPage>
  );
}

export function generateStaticParams() {
  const pages = source.generateParams();
  const pageSlugs = new Set(pages.map(({ slug }) => slug?.join('/') ?? ''));
  const redirectPages = Object.keys(legacyRedirects)
    .filter((slug) => !pageSlugs.has(slug))
    .map((slug) => ({ slug: slug.split('/') }));

  return [...pages, ...redirectPages];
}

export async function generateMetadata(props: PageProps): Promise<Metadata> {
  const params = await props.params;
  const redirect = getLegacyRedirect(params.slug);
  const pageSlug = redirect ? redirect.split('/').filter(Boolean) : params.slug;
  const slug = pageSlug?.join('/');
  const page = source.getPage(pageSlug);
  if (!page) notFound();

  const tutorialOgImage = slug ? tutorialOgImages[slug] : undefined;
  const pageOgImage = tutorialOgImage
    ? {
        url: withBasePath(tutorialOgImage),
        width: 1200,
        height: 630,
        type: 'image/png',
        alt: page.data.title,
      }
    : slug && slug in pageOgImages
      ? withBasePath(pageOgImages[slug])
      : ogImage;
  const pageUrl = withBasePath(page.url.endsWith('/') ? page.url : `${page.url}/`);

  return {
    title: page.data.title,
    description: page.data.description,
    alternates: {
      canonical: pageUrl,
    },
    openGraph: {
      type: 'website',
      siteName: 'WendyOS Docs',
      title: page.data.title,
      description: page.data.description,
      url: pageUrl,
      images: pageOgImage,
    },
    twitter: {
      card: 'summary_large_image',
      title: page.data.title,
      description: page.data.description,
      images: pageOgImage,
    },
  };
}
