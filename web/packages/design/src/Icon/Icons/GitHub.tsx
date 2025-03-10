/*
Copyright 2023 Gravitational, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

/* MIT License

Copyright (c) 2020 Phosphor Icons

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.

*/

import React from 'react';

import { Icon, IconProps } from '../Icon';

/*

THIS FILE IS GENERATED. DO NOT EDIT.

*/

export function GitHub({ size = 24, color, ...otherProps }: IconProps) {
  return (
    <Icon
      size={size}
      color={color}
      className="icon icon-github"
      {...otherProps}
    >
      <path
        fillRule="evenodd"
        clipRule="evenodd"
        d="M7.12482 2.25C8.06954 2.24978 8.9991 2.4875 9.82773 2.94123C10.5329 3.32738 11.1457 3.85939 11.6261 4.5H13.8739C14.3543 3.85939 14.9671 3.32738 15.6723 2.94123C16.5009 2.4875 17.4305 2.24978 18.3752 2.25C18.6429 2.25006 18.8902 2.39278 19.0242 2.62449C19.4445 3.35119 19.6966 4.16285 19.762 4.9998C19.8173 5.70844 19.7376 6.41996 19.5282 7.09681C19.9929 7.89994 20.2427 8.81191 20.25 9.74414L20.25 9.75L20.25 10.5C20.25 11.8924 19.6969 13.2277 18.7123 14.2123C17.8973 15.0273 16.842 15.5467 15.7129 15.7014C16.2204 16.3555 16.5 17.1633 16.5 18V21.75C16.5 22.1642 16.1642 22.5 15.75 22.5C15.3358 22.5 15 22.1642 15 21.75V18C15 17.4033 14.7629 16.831 14.341 16.409C13.919 15.9871 13.3467 15.75 12.75 15.75C12.1533 15.75 11.581 15.9871 11.159 16.409C10.7371 16.831 10.5 17.4033 10.5 18V21.75C10.5 22.1642 10.1642 22.5 9.75 22.5C9.33579 22.5 9 22.1642 9 21.75V20.25H6.75C5.75544 20.25 4.80161 19.8549 4.09835 19.1516C3.39509 18.4484 3 17.4946 3 16.5C3 15.9033 2.76295 15.331 2.34099 14.909C1.91903 14.4871 1.34674 14.25 0.75 14.25C0.335786 14.25 0 13.9142 0 13.5C0 13.0858 0.335786 12.75 0.75 12.75C1.74456 12.75 2.69839 13.1451 3.40165 13.8484C4.10491 14.5516 4.5 15.5054 4.5 16.5C4.5 17.0967 4.73705 17.669 5.15901 18.091C5.58097 18.5129 6.15326 18.75 6.75 18.75H9V18C9 17.1633 9.27961 16.3555 9.78707 15.7014C8.658 15.5467 7.60267 15.0273 6.78769 14.2123C5.80312 13.2277 5.25 11.8924 5.25 10.5V9.74414C5.25728 8.81191 5.50709 7.89994 5.97176 7.09681C5.76243 6.41996 5.68268 5.70844 5.73801 4.9998C5.80336 4.16285 6.05546 3.35119 6.47578 2.62449C6.6098 2.39278 6.85715 2.25006 7.12482 2.25ZM15 14.25H10.5C9.50544 14.25 8.55161 13.8549 7.84835 13.1517C7.14509 12.4484 6.75 11.4946 6.75 10.5V9.75306C6.75652 8.98906 6.98903 8.24406 7.41827 7.61196C7.55651 7.4084 7.5861 7.14997 7.49745 6.92043C7.27577 6.34642 7.18556 5.73002 7.23346 5.11655C7.26972 4.65213 7.38443 4.19833 7.57171 3.77413C8.10886 3.8325 8.63084 3.996 9.10731 4.2569C9.71496 4.58964 10.229 5.07006 10.6021 5.65385C10.7399 5.8695 10.9781 6 11.2341 6H14.2659C14.5219 6 14.7601 5.8695 14.8979 5.65385C15.271 5.07006 15.785 4.58964 16.3927 4.2569C16.8692 3.996 17.3911 3.8325 17.9283 3.77413C18.1156 4.19833 18.2303 4.65213 18.2665 5.11655C18.3144 5.73002 18.2242 6.34642 18.0025 6.92043C17.9139 7.14997 17.9435 7.4084 18.0817 7.61196C18.511 8.24406 18.7435 8.98906 18.75 9.75306V10.5C18.75 11.4946 18.3549 12.4484 17.6517 13.1517C16.9484 13.8549 15.9946 14.25 15 14.25Z"
      />
    </Icon>
  );
}
