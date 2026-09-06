import{bH as v,c as A,bL as a}from"./index-DNR7cWZ-.js";/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const h=t=>t==="";/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const L=(...t)=>t.filter((e,r,o)=>!!e&&e.trim()!==""&&o.indexOf(e)===r).join(" ").trim();/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const w=t=>t.replace(/([a-z0-9])([A-Z])/g,"$1-$2").toLowerCase();/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const S=t=>t.replace(/^([A-Z])|[\s-_]+(\w)/g,(e,r,o)=>o?o.toUpperCase():r.toLowerCase());/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const W=t=>{const e=S(t);return e.charAt(0).toUpperCase()+e.slice(1)};/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */var s={xmlns:"http://www.w3.org/2000/svg",width:24,height:24,viewBox:"0 0 24 24",fill:"none",stroke:"currentColor","stroke-width":2,"stroke-linecap":"round","stroke-linejoin":"round"};/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const $=Symbol("lucide-icons");function j(){return v($,{})}/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const E=({name:t,iconNode:e,absoluteStrokeWidth:r,"absolute-stroke-width":o,strokeWidth:c,"stroke-width":k,size:i,color:C,...f},{slots:l})=>{const{size:n,color:p,strokeWidth:m=2,absoluteStrokeWidth:g=!1,class:x=""}=j(),b=A(()=>{const u=h(r)||h(o)||r===!0||o===!0||g===!0,d=c||k||m||s["stroke-width"];return u?Number(d)*24/Number(i??n??s.width):d});return a("svg",{...s,...f,width:i??n??s.width,height:i??n??s.height,stroke:C??p??s.stroke,"stroke-width":b.value,class:L("lucide",x,...t?[`lucide-${w(W(t))}-icon`,`lucide-${w(t)}`]:["lucide-icon"])},[...e.map(u=>a(...u)),...l.default?[l.default()]:[]])};/**
 * @license @lucide/vue v1.23.0 - ISC
 *
 * This source code is licensed under the ISC license.
 * See the LICENSE file in the root directory of this source tree.
 */const U=(t,e)=>(r,{slots:o,attrs:c})=>a(E,{...c,...r,iconNode:e,name:t},o.default?{default:o.default}:void 0);export{U as c};
