/*
Copyright (C) 2023-2026 QuantumNous

This program is free software: you can redistribute it and/or modify
it under the terms of the GNU Affero General Public License as
published by the Free Software Foundation, either version 3 of the
License, or (at your option) any later version.

This program is distributed in the hope that it will be useful,
but WITHOUT ANY WARRANTY; without even the implied warranty of
MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
GNU Affero General Public License for more details.

You should have received a copy of the GNU Affero General Public License
along with this program. If not, see <https://www.gnu.org/licenses/>.

For commercial licensing, please contact support@quantumnous.com
*/
import { act, render, screen } from '@testing-library/react'
import { createInstance } from 'i18next'
import { I18nextProvider, initReactI18next } from 'react-i18next'
import { expect, it } from 'vitest'

import en from '@/i18n/locales/en.json'
import fr from '@/i18n/locales/fr.json'
import ja from '@/i18n/locales/ja.json'
import ru from '@/i18n/locales/ru.json'
import vi from '@/i18n/locales/vi.json'
import zhTW from '@/i18n/locales/zh-TW.json'
import zh from '@/i18n/locales/zh.json'

import type { SystemStatus } from '../../types'
import { LegalConsent } from '../legal-consent'
import { TermsFooter } from '../terms-footer'

const enabledStatus = {
  user_agreement_enabled: true,
  privacy_policy_enabled: true,
}
const signInText = 'By clicking sign in, you agree to our'
const signUpText = 'By creating an account, you agree to our'

async function createLegalI18n(language: string) {
  const i18n = createInstance()
  await i18n.use(initReactI18next).init({
    lng: language,
    fallbackLng: false,
    resources: { en, fr, ja, ru, vi, zh, 'zh-TW': zhTW },
    interpolation: { escapeValue: false },
  })
  return i18n
}

it.each([
  ['sign-in', '登录即表示您同意我们的'],
  ['sign-up', '创建账户即表示您同意我们的'],
] as const)(
  'renders the %s footer entirely in Chinese',
  async (variant, intro) => {
    const i18n = await createLegalI18n('zh')
    const view = render(
      <I18nextProvider i18n={i18n}>
        <TermsFooter variant={variant} status={enabledStatus} />
      </I18nextProvider>
    )

    expect(view.container).toHaveTextContent(`${intro} 用户协议 和 隐私政策.`)
    expect(screen.getByRole('link', { name: '用户协议' })).toHaveAttribute(
      'href',
      '/user-agreement'
    )
    expect(screen.getByRole('link', { name: '隐私政策' })).toHaveAttribute(
      'href',
      '/privacy-policy'
    )
  }
)

it.each([
  ['en', 'and'],
  ['zh', '和'],
  ['zh-TW', '和'],
  ['fr', 'et'],
  ['ja', 'および'],
  ['ru', 'и'],
  ['vi', 'và'],
])(
  'localizes both footer variants and consent in %s',
  async (language, and) => {
    const i18n = await createLegalI18n(language)
    const view = render(
      <I18nextProvider i18n={i18n}>
        <TermsFooter variant='sign-in' status={enabledStatus} />
        <TermsFooter variant='sign-up' status={enabledStatus} />
        <LegalConsent
          status={enabledStatus}
          checked={false}
          onCheckedChange={() => undefined}
        />
      </I18nextProvider>
    )

    const agreement = i18n.t('User Agreement')
    const privacy = i18n.t('Privacy Policy')
    for (const key of [signInText, signUpText]) {
      expect(i18n.exists(key)).toBe(true)
      if (language !== 'en') expect(i18n.t(key)).not.toBe(key)
      expect(view.container).toHaveTextContent(
        `${i18n.t(key)} ${agreement} ${and} ${privacy}.`
      )
    }
    expect(screen.getAllByRole('link', { name: agreement })).toHaveLength(3)
    expect(screen.getAllByRole('link', { name: privacy })).toHaveLength(3)
    expect(screen.getByRole('checkbox')).toHaveAccessibleName(
      `${i18n.t('I have read and agree to the')} ${agreement} ${and} ${privacy}.`
    )
  }
)

it.each<{
  name: string
  status?: SystemStatus | null
  links: string[]
}>([
  { name: 'missing status', links: [] },
  { name: 'null status', status: null, links: [] },
  { name: 'disabled documents', status: {}, links: [] },
  {
    name: 'only the agreement enabled',
    status: { user_agreement_enabled: true, privacy_policy_enabled: false },
    links: ['用户协议'],
  },
  {
    name: 'only the privacy policy enabled',
    status: { user_agreement_enabled: false, privacy_policy_enabled: true },
    links: ['隐私政策'],
  },
])(
  'renders only configured documents with $name',
  async ({ status, links }) => {
    const i18n = await createLegalI18n('zh')
    const view = render(
      <I18nextProvider i18n={i18n}>
        <TermsFooter status={status} />
        <LegalConsent
          status={status ?? null}
          checked={false}
          onCheckedChange={() => undefined}
        />
      </I18nextProvider>
    )

    expect(
      screen.queryAllByRole('link').map((link) => link.textContent)
    ).toEqual([...links, ...links])
    if (links.length === 0) {
      expect(view.container).toBeEmptyDOMElement()
    } else {
      expect(view.container).not.toHaveTextContent('和')
    }
  }
)

it('updates footer and consent text when the language changes', async () => {
  const i18n = await createLegalI18n('en')
  const view = render(
    <I18nextProvider i18n={i18n}>
      <TermsFooter status={enabledStatus} />
      <LegalConsent
        status={enabledStatus}
        checked={false}
        onCheckedChange={() => undefined}
      />
    </I18nextProvider>
  )
  expect(view.container).toHaveTextContent(signInText)
  expect(screen.getAllByRole('link', { name: 'User Agreement' })).toHaveLength(
    2
  )

  await act(() => i18n.changeLanguage('zh'))

  expect(view.container).toHaveTextContent('登录即表示您同意我们的')
  expect(screen.getAllByRole('link', { name: '用户协议' })).toHaveLength(2)
  expect(screen.getByRole('checkbox')).toHaveAccessibleName(
    '我已阅读并同意 用户协议 和 隐私政策.'
  )
  expect(view.container).not.toHaveTextContent(signInText)
})
