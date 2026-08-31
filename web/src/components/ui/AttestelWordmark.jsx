// AttestelWordmark — the product wordmark, inlined.
//
// Same outlines as the approved asset in `web/public/brand/attestel-wordmark.svg` and the
// landing page's nav logo, so the two surfaces render identically. Inlined rather than loaded from
// /brand so it costs no request, survives the zero-network/offline path, and is independent of the
// Vite base path (the app is served from /app/ in production).
//
// `fill="currentColor"` — colour comes from the surrounding text colour, as everywhere else.
// Sized by height; width follows the 3729:826 viewBox aspect ratio.

export function AttestelWordmark({ height = 18, className = "" }) {
  return (
    <svg
      height={height}
      viewBox="0 0 3729 826"
      xmlns="http://www.w3.org/2000/svg"
      role="img"
      aria-label="Attestel"
      className={className}
      style={{ width: (height * 3729) / 826 }}
    >
      <g fill="currentColor" transform="translate(25 770) scale(1 -1)">
        {/* A */}
        <path
          d="M15 0L285 716H410L681 0H560L378 505L351 587H345L318 505L136 0ZM339 184V286H505L541 184ZM144 184L180 286H339V184Z" />
        {/* t */}
        <path transform="translate(696 0)"
          d="M25 416V510H120V416ZM270 -8Q199 -8 157 34Q115 76 115 148V654H223V171Q223 131 239 112Q255 93 290 93Q306 93 320 97.5Q334 102 351 112V7Q332 0 312 -4Q292 -8 270 -8ZM217 416V510H347V416Z" />
        {/* t */}
        <path transform="translate(1085 0)"
          d="M25 416V510H120V416ZM270 -8Q199 -8 157 34Q115 76 115 148V654H223V171Q223 131 239 112Q255 93 290 93Q306 93 320 97.5Q334 102 351 112V7Q332 0 312 -4Q292 -8 270 -8ZM217 416V510H347V416Z" />
        {/* e */}
        <path transform="translate(1474 0)"
          d="M295 -16Q220 -16 161 19Q102 54 68.5 115Q35 176 35 254Q35 327 67 389Q99 451 156.5 488.5Q214 526 289 526Q368 526 423.5 492Q479 458 508 399Q537 340 537 266Q537 255 536.5 246Q536 237 535 232H96V313H430Q429 332 420 353.5Q411 375 394 393Q377 411 351 422.5Q325 434 290 434Q246 434 212 411.5Q178 389 159 349Q140 309 140 256Q140 198 162 159Q184 120 220 100Q256 80 298 80Q349 80 383.5 103.5Q418 127 438 162L527 119Q494 59 437 21.5Q380 -16 295 -16Z" />
        {/* s */}
        <path transform="translate(2046 0)"
          d="M248 -16Q187 -16 142 2.5Q97 21 68 52.5Q39 84 25 121L122 163Q140 122 173.5 100.5Q207 79 251 79Q291 79 320 93.5Q349 108 349 141Q349 162 335.5 175.5Q322 189 299.5 198Q277 207 248 214L187 228Q151 237 118.5 256.5Q86 276 66 306Q46 336 46 377Q46 423 72.5 456.5Q99 490 144 508Q189 526 241 526Q289 526 328.5 513.5Q368 501 397.5 476.5Q427 452 444 416L351 374Q334 408 305 421.5Q276 435 242 435Q204 435 178.5 419.5Q153 404 153 379Q153 353 175.5 338Q198 323 231 314L305 296Q381 277 419 238.5Q457 200 457 145Q457 96 429 59.5Q401 23 353.5 3.5Q306 -16 248 -16Z" />
        {/* t */}
        <path transform="translate(2533 0)"
          d="M25 416V510H120V416ZM270 -8Q199 -8 157 34Q115 76 115 148V654H223V171Q223 131 239 112Q255 93 290 93Q306 93 320 97.5Q334 102 351 112V7Q332 0 312 -4Q292 -8 270 -8ZM217 416V510H347V416Z" />
        {/* e */}
        <path transform="translate(2922 0)"
          d="M295 -16Q220 -16 161 19Q102 54 68.5 115Q35 176 35 254Q35 327 67 389Q99 451 156.5 488.5Q214 526 289 526Q368 526 423.5 492Q479 458 508 399Q537 340 537 266Q537 255 536.5 246Q536 237 535 232H96V313H430Q429 332 420 353.5Q411 375 394 393Q377 411 351 422.5Q325 434 290 434Q246 434 212 411.5Q178 389 159 349Q140 309 140 256Q140 198 162 159Q184 120 220 100Q256 80 298 80Q349 80 383.5 103.5Q418 127 438 162L527 119Q494 59 437 21.5Q380 -16 295 -16Z" />
        {/* l */}
        <path transform="translate(3494 0)"
          d="M62 0V716H170V0Z" />
      </g>
    </svg>
  );
}
